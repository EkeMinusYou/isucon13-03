package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

var currentMutations atomic.Pointer[postStore]
var appLifecycle sync.RWMutex

type mutationOperation interface{ mutationOperation() }
type mutationBarrier interface {
	mutationOperation
	apply(context.Context, *sqlx.Tx) error
}
type commentMutation struct {
	model   *LivecommentModel
	ownerID int64
}

func (*commentMutation) mutationOperation() {}

type reactionMutation struct{ model *ReactionModel }

func (*reactionMutation) mutationOperation() {}

func submitMutation(ctx context.Context, stream int64, op mutationOperation) error {
	s := currentMutations.Load()
	if s == nil {
		return postsUnavailable("post store is not initialized")
	}
	return s.submit(ctx, stream, op)
}

func (s *postStore) submit(ctx context.Context, stream int64, op mutationOperation) error {
	s.life.RLock()
	defer s.life.RUnlock()
	if err := s.check(); err != nil {
		return err
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.slots }()
	state, err := s.stream(stream)
	if err != nil {
		return err
	}
	if err := state.acquire(ctx); err != nil {
		return err
	}
	defer state.release()
	if err := s.check(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var record postRecord
	switch op := op.(type) {
	case *commentMutation:
		m := op.model
		if !utf8.ValidString(m.Comment) || utf8.RuneCountInString(m.Comment) > 255 {
			return fmt.Errorf("comment exceeds VARCHAR(255)")
		}
		var spam bool
		if err := s.db.GetContext(ctx, &spam, `SELECT EXISTS(SELECT 1 FROM ng_words WHERE user_id=? AND livestream_id=? AND CONVERT(? USING utf8mb4) COLLATE utf8mb4_general_ci LIKE CONCAT('%', word COLLATE utf8mb4_general_ci, '%'))`, op.ownerID, m.LivestreamID, m.Comment); err != nil {
			return err
		}
		if spam {
			return echo.NewHTTPError(http.StatusBadRequest, "このコメントがスパム判定されました")
		}
		record = postRecord{Kind: "comment", UserID: m.UserID, StreamID: m.LivestreamID, Text: m.Comment, Tip: m.Tip, CreatedAt: m.CreatedAt}
	case *reactionMutation:
		m := op.model
		if !utf8.ValidString(m.EmojiName) || utf8.RuneCountInString(m.EmojiName) > 255 {
			return fmt.Errorf("emoji exceeds VARCHAR(255)")
		}
		record = postRecord{Kind: "reaction", UserID: m.UserID, StreamID: m.LivestreamID, Text: m.EmojiName, CreatedAt: m.CreatedAt}
	default:
		return s.applyBarrier(ctx, state, op.(mutationBarrier))
	}
	if err := s.reserveRow(record.cost()); err != nil {
		return err
	}
	req := postRequest{record: record, done: make(chan postResult, 1)}
	select {
	case s.requests <- req:
	case <-ctx.Done():
		s.releaseRow(record.cost())
		return ctx.Err()
	}
	// Once accepted, cancellation cannot abandon publication of a durable row.
	result := <-req.done
	if result.err != nil {
		s.releaseRow(record.cost())
		return result.err
	}
	r := result.record
	if r.Kind == "comment" {
		state.insertComment(r.comment())
		op.(*commentMutation).model.ID = r.ID
	} else {
		state.insertReaction(r.reaction())
		op.(*reactionMutation).model.ID = r.ID
	}
	state.lastSeq = r.Seq
	return nil
}

func (s *postStore) applyBarrier(ctx context.Context, state *postStream, op mutationBarrier) error {
	through := state.lastSeq
	if _, ok := op.(*reportMutation); ok {
		through = s.durableSeq.Load()
	}
	if _, ok := op.(*viewerMutation); !ok {
		if err := s.flush(ctx, through); err != nil {
			return err
		}
	}
	// Client disconnection does not interrupt a transaction after its admission.
	work, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTxx(work, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := op.apply(work, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		s.fail(fmt.Errorf("mutation COMMIT outcome unknown: %w", err))
		return err
	}
	if moderation, ok := op.(*moderationMutation); ok {
		state.rowsMu.Lock()
		defer state.rowsMu.Unlock()
		deleted := make(map[int64]bool, len(moderation.deleted))
		for _, id := range moderation.deleted {
			deleted[id] = true
		}
		remaining := state.comments[:0]
		for _, row := range state.comments {
			if deleted[row.ID] {
				s.releaseRow(commentRecord(row).cost())
			} else {
				remaining = append(remaining, row)
			}
		}
		clear(state.comments[len(remaining):])
		// Release unused backing capacity along with the deleted rows' budget.
		state.comments = append([]LivecommentModel{}, remaining...)
	}
	return nil
}

func flushPosts(ctx context.Context) error {
	s := currentMutations.Load()
	if s == nil {
		return postsUnavailable("post store is not initialized")
	}
	return s.flush(ctx, s.durableSeq.Load())
}

func mutationHTTPError(err error) error {
	var httpErr *echo.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr
	}
	return echo.NewHTTPError(http.StatusInternalServerError, "failed to write mutation: "+err.Error())
}

func applicationLifecycle(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if c.Path() == "/api/initialize" {
			return next(c)
		}
		appLifecycle.RLock()
		defer appLifecycle.RUnlock()
		if currentMutations.Load() == nil {
			return postsUnavailable("initialization is required")
		}
		return next(c)
	}
}
