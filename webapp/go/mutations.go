package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"
)

var currentMutations atomic.Pointer[postStore]
var appLifecycle sync.RWMutex

type mutationOperation interface{ mutationOperation() }
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
		state.rowsMu.RLock()
		patterns := state.ngPatterns[op.ownerID]
		spam := matchesNG(m.Comment, patterns)
		state.rowsMu.RUnlock()
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
	case *moderationMutation:
		record, err = s.prepareModeration(ctx, state, op)
		if err != nil {
			return err
		}
	case *reportMutation:
		record, err = s.prepareReport(ctx, op)
		if err != nil {
			return err
		}
	case *viewerMutation:
		kind := "exit"
		if op.enter {
			kind = "enter"
		}
		record = postRecord{Kind: kind, UserID: op.userID, StreamID: op.livestreamID, CreatedAt: time.Now().Unix()}
	default:
		return fmt.Errorf("unknown mutation")
	}
	if record.cost() > 0 {
		if err := s.reserveRow(record.cost()); err != nil {
			return err
		}
	}
	req := postRequest{record: record, done: make(chan postResult, 1)}
	select {
	case s.requests <- req:
	case <-ctx.Done():
		if record.cost() > 0 {
			s.releaseRow(record.cost())
		}
		return ctx.Err()
	}
	// Once accepted, cancellation cannot abandon the complete write and publication.
	result := <-req.done
	if result.err != nil {
		if record.cost() > 0 {
			s.releaseRow(record.cost())
		}
		return result.err
	}
	r := result.record
	switch op := op.(type) {
	case *commentMutation:
		op.model.ID = r.ID
	case *reactionMutation:
		op.model.ID = r.ID
	case *moderationMutation:
		*op.wordID = r.ID
	case *reportMutation:
		op.response.ID = r.ID
	}
	return nil
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
