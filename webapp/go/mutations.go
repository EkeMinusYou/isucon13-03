package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

const mutationWriterCount = 4

type mutationRequest struct {
	ctx  context.Context
	op   mutationOperation
	done chan error
}

type mutationWriter struct {
	db         *sqlx.DB
	queues     [mutationWriterCount]chan mutationRequest
	workers    sync.WaitGroup
	mu         sync.RWMutex
	closed     bool
	commits    atomic.Uint64
	operations atomic.Uint64
}

var currentMutations atomic.Pointer[mutationWriter]

func newMutationWriter(db *sqlx.DB) *mutationWriter {
	w := &mutationWriter{db: db}
	for i := range w.queues {
		w.queues[i] = make(chan mutationRequest, 256/mutationWriterCount)
		w.workers.Add(1)
		go w.run(w.queues[i])
	}
	return w
}

func (w *mutationWriter) submit(ctx context.Context, livestreamID int64, op mutationOperation) error {
	r := mutationRequest{ctx: ctx, op: op, done: make(chan error, 1)}
	// All mutations for a stream use one FIFO, including moderation and reports.
	queue := w.queues[uint64(livestreamID)%mutationWriterCount]
	w.mu.RLock()
	if w.closed {
		w.mu.RUnlock()
		return fmt.Errorf("mutation writer is stopped")
	}
	select {
	case queue <- r:
		w.mu.RUnlock()
	case <-ctx.Done():
		w.mu.RUnlock()
		return ctx.Err()
	}
	select {
	case err := <-r.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *mutationWriter) close() {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		for _, queue := range w.queues {
			close(queue)
		}
	}
	w.mu.Unlock()
	w.workers.Wait()
}

func (w *mutationWriter) run(queue <-chan mutationRequest) {
	defer w.workers.Done()
	for first := range queue {
		batch := []mutationRequest{first}
		timer := time.NewTimer(time.Millisecond)
	collect:
		for len(batch) < 32 {
			select {
			case r, ok := <-queue:
				if !ok {
					break collect
				}
				batch = append(batch, r)
			case <-timer.C:
				break collect
			}
		}
		timer.Stop()
		w.write(batch)
	}
}

func (w *mutationWriter) write(batch []mutationRequest) {
	// A disconnected client must not cancel another request's transaction.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Stream-local ordering replaces cross-stream range locking.
	// READ COMMITTED keeps a moderation DELETE from gap-locking another stream's INSERT.
	tx, err := w.db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		for _, r := range batch {
			r.done <- err
		}
		return
	}
	defer tx.Rollback()
	results := make([]error, len(batch))
	var failure error
	for start := 0; start < len(batch) && failure == nil; {
		end := start
		for end < len(batch) && isPostMutation(batch[end].op) {
			end++
		}
		if end > start {
			failure = writePosts(ctx, tx, batch[start:end], results[start:end])
			start = end
			continue
		}
		r := batch[start]
		if err := r.ctx.Err(); err != nil {
			results[start] = err
		} else {
			failure = writeBarrier(ctx, tx, r.op.(mutationBarrier), &results[start])
		}
		start++
	}
	if failure == nil {
		failure = tx.Commit()
		if failure == nil {
			w.commits.Add(1)
			w.operations.Add(uint64(len(batch)))
		}
	}
	for i, r := range batch {
		if failure != nil {
			r.done <- fmt.Errorf("commit mutation batch: %w", failure)
		} else {
			r.done <- results[i]
		}
	}
}

func submitMutation(ctx context.Context, livestreamID int64, op mutationOperation) error {
	w := currentMutations.Load()
	if w == nil {
		return fmt.Errorf("mutation writer is not initialized")
	}
	return w.submit(ctx, livestreamID, op)
}

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

func isPostMutation(op mutationOperation) bool {
	switch op.(type) {
	case *commentMutation, *reactionMutation:
		return true
	}
	return false
}

// Infrastructure failures invalidate the transaction, including earlier groups.
func mutationTransactionFailure(err error) bool {
	var httpErr *echo.HTTPError
	if errors.As(err, &httpErr) {
		return false
	}
	var sqlErr *mysql.MySQLError
	if errors.As(err, &sqlErr) {
		switch sqlErr.Number {
		case 1048, 1062, 1264, 1366, 1406, 1451, 1452, 3819:
			return false
		}
	}
	return true
}

func writeBarrier(ctx context.Context, tx *sqlx.Tx, op mutationBarrier, result *error) error {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT request_operation"); err != nil {
		return err
	}
	if err := op.apply(ctx, tx); err != nil {
		if mutationTransactionFailure(err) {
			return err
		}
		if _, restoreErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT request_operation"); restoreErr != nil {
			return restoreErr
		}
		*result = err
	}
	return nil
}

func writePosts(ctx context.Context, tx *sqlx.Tx, batch []mutationRequest, results []error) error {
	var comments, reactions []int
	for i, r := range batch {
		if err := r.ctx.Err(); err != nil {
			results[i] = err
			continue
		}
		switch r.op.(type) {
		case *commentMutation:
			comments = append(comments, i)
		case *reactionMutation:
			reactions = append(reactions, i)
		}
	}
	if len(comments) > 0 {
		parts := make([]string, len(comments))
		args := make([]interface{}, 0, len(comments)*3)
		for j, i := range comments {
			op := batch[i].op.(*commentMutation)
			parts[j] = fmt.Sprintf("SELECT %d AS request_index, EXISTS(SELECT 1 FROM ng_words WHERE user_id = ? AND livestream_id = ? AND CONVERT(? USING utf8mb4) COLLATE utf8mb4_general_ci LIKE CONCAT('%%', word COLLATE utf8mb4_general_ci, '%%')) AS spam", i)
			args = append(args, op.ownerID, op.model.LivestreamID, op.model.Comment)
		}
		var matches []struct {
			Index int  `db:"request_index"`
			Spam  bool `db:"spam"`
		}
		if err := tx.SelectContext(ctx, &matches, strings.Join(parts, " UNION ALL "), args...); err != nil {
			return err
		}
		if len(matches) != len(comments) {
			return fmt.Errorf("incomplete batch NG result")
		}
		for _, match := range matches {
			if match.Spam {
				results[match.Index] = echo.NewHTTPError(http.StatusBadRequest, "このコメントがスパム判定されました")
			}
		}
		accepted := comments[:0]
		for _, i := range comments {
			if results[i] == nil {
				accepted = append(accepted, i)
			}
		}
		if err := insertPosts(ctx, tx, batch, accepted, results, true); err != nil {
			return err
		}
	}
	return insertPosts(ctx, tx, batch, reactions, results, false)
}

// Simple VALUES inserts allocate one consecutive ID block per statement.
// A failed group is split only for row-data errors, preserving healthy requests.
func insertPosts(ctx context.Context, tx *sqlx.Tx, batch []mutationRequest, indices []int, results []error, comments bool) error {
	if len(indices) == 0 {
		return nil
	}
	query := "INSERT INTO reactions (user_id,livestream_id,emoji_name,created_at) VALUES "
	values := "(?,?,?,?)"
	if comments {
		query = "INSERT INTO livecomments (user_id,livestream_id,comment,tip,created_at) VALUES "
		values = "(?,?,?,?,?)"
	}
	parts := make([]string, len(indices))
	args := make([]interface{}, 0, len(indices)*5)
	for j, i := range indices {
		parts[j] = values
		if comments {
			m := batch[i].op.(*commentMutation).model
			args = append(args, m.UserID, m.LivestreamID, m.Comment, m.Tip, m.CreatedAt)
		} else {
			m := batch[i].op.(*reactionMutation).model
			args = append(args, m.UserID, m.LivestreamID, m.EmojiName, m.CreatedAt)
		}
	}
	if _, err := tx.ExecContext(ctx, "SAVEPOINT post_group"); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, query+strings.Join(parts, ","), args...)
	if err != nil {
		if mutationTransactionFailure(err) {
			return err
		}
		if _, restoreErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT post_group"); restoreErr != nil {
			return restoreErr
		}
		if len(indices) == 1 {
			results[indices[0]] = err
			return nil
		}
		middle := len(indices) / 2
		if err := insertPosts(ctx, tx, batch, indices[:middle], results, comments); err != nil {
			return err
		}
		return insertPosts(ctx, tx, batch, indices[middle:], results, comments)
	}
	firstID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != int64(len(indices)) || firstID <= 0 || firstID > int64(^uint64(0)>>1)-int64(len(indices)-1) {
		return fmt.Errorf("unexpected batch INSERT result")
	}
	for j, i := range indices {
		if comments {
			batch[i].op.(*commentMutation).model.ID = firstID + int64(j)
		} else {
			batch[i].op.(*reactionMutation).model.ID = firstID + int64(j)
		}
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
