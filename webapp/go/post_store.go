package main

import (
	"context"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

const (
	postRowLimit     = 500000
	postMemoryLimit  = 256 << 20
	postWALLimit     = 512 << 20
	postPendingLimit = 16 << 20
)

type postStream struct {
	rowsMu    sync.RWMutex
	gate      chan struct{}
	comments  []LivecommentModel
	reactions []ReactionModel
	lastSeq   uint64
}

func (s *postStream) acquire(ctx context.Context) error {
	select {
	case s.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *postStream) release() { <-s.gate }
func (s *postStream) insertComment(row LivecommentModel) {
	s.rowsMu.Lock()
	defer s.rowsMu.Unlock()
	i := sort.Search(len(s.comments), func(i int) bool {
		r := s.comments[i]
		return r.CreatedAt < row.CreatedAt || (r.CreatedAt == row.CreatedAt && r.ID < row.ID)
	})
	s.comments = append(s.comments, LivecommentModel{})
	copy(s.comments[i+1:], s.comments[i:])
	s.comments[i] = row
}
func (s *postStream) insertReaction(row ReactionModel) {
	s.rowsMu.Lock()
	defer s.rowsMu.Unlock()
	i := sort.Search(len(s.reactions), func(i int) bool {
		r := s.reactions[i]
		return r.CreatedAt < row.CreatedAt || (r.CreatedAt == row.CreatedAt && r.ID < row.ID)
	})
	s.reactions = append(s.reactions, ReactionModel{})
	copy(s.reactions[i+1:], s.reactions[i:])
	s.reactions[i] = row
}

type postStore struct {
	db                         *sqlx.DB
	generation                 string
	file, reader               *os.File
	capacity                   int64
	life                       sync.RWMutex
	closed                     bool
	syncFile                   func() error
	mu                         sync.Mutex
	err                        error
	changed                    chan struct{}
	streams                    map[int64]*postStream
	rows, memory               int64
	slots                      chan struct{}
	requests                   chan postRequest
	wake                       chan struct{}
	stop                       chan struct{}
	writers, projectors        sync.WaitGroup
	durableSeq, appliedSeq     atomic.Uint64
	durableBytes, appliedBytes atomic.Int64
	syncs, syncNanos, records  atomic.Uint64
	nextComment, nextReaction  int64
	walMu                      sync.Mutex
	pending                    []postBatch
	syncWake                   chan struct{}
	writtenSeq                 uint64
	writtenBytes               int64
}

func postsUnavailable(reason string) error {
	return echo.NewHTTPError(http.StatusServiceUnavailable, reason)
}
func (s *postStore) check() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return postsUnavailable("post store is closed")
	}
	if s.err != nil {
		return postsUnavailable("post store failed: " + s.err.Error())
	}
	return nil
}
func (s *postStore) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
		close(s.changed)
		s.changed = make(chan struct{})
	}
}
func (s *postStore) stream(id int64) (*postStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state := s.streams[id]; state != nil {
		return state, nil
	}
	if s.memory+512 > postMemoryLimit {
		return nil, postsUnavailable("post memory limit reached")
	}
	state := &postStream{gate: make(chan struct{}, 1)}
	s.streams[id] = state
	s.memory += 512
	return state, nil
}
func (s *postStore) reserveRow(cost int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows >= postRowLimit || s.memory+cost > postMemoryLimit {
		return postsUnavailable("post memory limit reached")
	}
	s.rows++
	s.memory += cost
	return nil
}
func (s *postStore) releaseRow(cost int64) { s.mu.Lock(); s.rows--; s.memory -= cost; s.mu.Unlock() }

func (s *postStore) timeline(ctx context.Context, stream int64, limit int, comments bool) ([]LivecommentModel, []ReactionModel, error) {
	s.life.RLock()
	defer s.life.RUnlock()
	if err := s.check(); err != nil {
		return nil, nil, err
	}
	state, err := s.stream(stream)
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	state.rowsMu.RLock()
	defer state.rowsMu.RUnlock()
	if err := s.check(); err != nil {
		return nil, nil, err
	}
	if comments {
		n := len(state.comments)
		if limit >= 0 && limit < n {
			n = limit
		}
		return append([]LivecommentModel{}, state.comments[:n]...), nil, nil
	}
	n := len(state.reactions)
	if limit >= 0 && limit < n {
		n = limit
	}
	return nil, append([]ReactionModel{}, state.reactions[:n]...), nil
}

func (s *postStore) load(ctx context.Context) error {
	var ids []int64
	if err := s.db.SelectContext(ctx, &ids, "SELECT id FROM livestreams"); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.stream(id); err != nil {
			return err
		}
	}
	var comments []LivecommentModel
	if err := s.db.SelectContext(ctx, &comments, "SELECT * FROM livecomments ORDER BY livestream_id,created_at DESC,id DESC"); err != nil {
		return err
	}
	for _, row := range comments {
		if err := s.reserveRow(commentRecord(row).cost()); err != nil {
			return err
		}
		state, err := s.stream(row.LivestreamID)
		if err != nil {
			return err
		}
		state.comments = append(state.comments, row)
		if row.ID >= s.nextComment {
			s.nextComment = row.ID + 1
		}
	}
	var reactions []ReactionModel
	if err := s.db.SelectContext(ctx, &reactions, "SELECT * FROM reactions ORDER BY livestream_id,created_at DESC,id DESC"); err != nil {
		return err
	}
	for _, row := range reactions {
		if err := s.reserveRow(reactionRecord(row).cost()); err != nil {
			return err
		}
		state, err := s.stream(row.LivestreamID)
		if err != nil {
			return err
		}
		state.reactions = append(state.reactions, row)
		if row.ID >= s.nextReaction {
			s.nextReaction = row.ID + 1
		}
	}
	var counters []struct {
		Name string `db:"TABLE_NAME"`
		Next int64  `db:"next_id"`
	}
	if err := s.db.SelectContext(ctx, &counters, "SELECT TABLE_NAME,COALESCE(AUTO_INCREMENT,1) AS next_id FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME IN ('livecomments','reactions')"); err != nil {
		return err
	}
	for _, counter := range counters {
		if counter.Name == "livecomments" && counter.Next > s.nextComment {
			s.nextComment = counter.Next
		}
		if counter.Name == "reactions" && counter.Next > s.nextReaction {
			s.nextReaction = counter.Next
		}
	}
	return nil
}

func (s *postStore) close() error {
	s.life.Lock()
	defer s.life.Unlock()
	s.mu.Lock()
	already := s.closed
	s.mu.Unlock()
	if already {
		return nil
	}
	close(s.requests)
	s.writers.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), postDBTimeout)
	err := s.flush(ctx, s.durableSeq.Load())
	cancel()
	close(s.stop)
	s.projectors.Wait()
	s.mu.Lock()
	s.closed = true
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
	if e := s.reader.Close(); err == nil {
		err = e
	}
	if e := s.file.Close(); err == nil {
		err = e
	}
	return err
}

func (s *postStore) metrics() map[string]any {
	s.mu.Lock()
	rows, memory, failed := s.rows, s.memory, s.err != nil
	durable, applied := s.durableSeq.Load(), s.appliedSeq.Load()
	walBytes, pending := s.durableBytes.Load(), s.durableBytes.Load()-s.appliedBytes.Load()
	s.mu.Unlock()
	return map[string]any{"generation": s.generation, "durable_seq": durable, "applied_seq": applied, "wal_bytes": walBytes, "pending_bytes": pending, "rows": rows, "model_budget_bytes": memory, "records": s.records.Load(), "syncs": s.syncs.Load(), "sync_ns": s.syncNanos.Load(), "failed": failed}
}

func postDirectory() string {
	if dir := os.Getenv("ISUPIPE_WAL_DIR"); dir != "" {
		return dir
	}
	return "/var/lib/isupipe/wal"
}

func ensurePostStream(id int64) error {
	s := currentMutations.Load()
	if s == nil {
		return postsUnavailable("post store is not initialized")
	}
	_, err := s.stream(id)
	return err
}
