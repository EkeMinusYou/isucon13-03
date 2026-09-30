package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

type mutationRequest struct {
	ctx   context.Context
	apply func(context.Context, *sqlx.Tx) error
	done  chan error
}

type mutationWriter struct {
	db         *sqlx.DB
	queue      chan mutationRequest
	done       chan struct{}
	mu         sync.RWMutex
	closed     bool
	commits    atomic.Uint64
	operations atomic.Uint64
}

var currentMutations atomic.Pointer[mutationWriter]

func newMutationWriter(db *sqlx.DB) *mutationWriter {
	w := &mutationWriter{db: db, queue: make(chan mutationRequest, 256), done: make(chan struct{})}
	go w.run()
	return w
}

func (w *mutationWriter) submit(ctx context.Context, apply func(context.Context, *sqlx.Tx) error) error {
	r := mutationRequest{ctx: ctx, apply: apply, done: make(chan error, 1)}
	w.mu.RLock()
	if w.closed {
		w.mu.RUnlock()
		return fmt.Errorf("mutation writer is stopped")
	}
	select {
	case w.queue <- r:
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
		close(w.queue)
	}
	w.mu.Unlock()
	<-w.done
}

func (w *mutationWriter) run() {
	defer close(w.done)
	for first := range w.queue {
		batch := []mutationRequest{first}
		timer := time.NewTimer(time.Millisecond)
	collect:
		for len(batch) < 32 {
			select {
			case r, ok := <-w.queue:
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
	tx, err := w.db.BeginTxx(ctx, nil)
	if err != nil {
		for _, r := range batch {
			r.done <- err
		}
		return
	}
	defer tx.Rollback()
	results := make([]error, len(batch))
	var failure error
	for i, r := range batch {
		if err := r.ctx.Err(); err != nil {
			results[i] = err
			continue
		}
		// Reusing the name replaces the preceding successful savepoint.
		if _, err := tx.ExecContext(ctx, "SAVEPOINT request_operation"); err != nil {
			failure = err
			break
		}
		if err := r.apply(ctx, tx); err != nil {
			results[i] = err
			if _, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT request_operation"); rollbackErr != nil {
				failure = fmt.Errorf("restore request savepoint: %w", rollbackErr)
				break
			}
		}
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

func submitMutation(ctx context.Context, apply func(context.Context, *sqlx.Tx) error) error {
	w := currentMutations.Load()
	if w == nil {
		return fmt.Errorf("mutation writer is not initialized")
	}
	return w.submit(ctx, apply)
}

func mutationHTTPError(err error) error {
	var httpErr *echo.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr
	}
	return echo.NewHTTPError(http.StatusInternalServerError, "failed to write mutation: "+err.Error())
}
