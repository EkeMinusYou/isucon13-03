package main

import (
	"context"
	"net/http"
	"os"
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
	words     []NGWord
	reports   []LivecommentReportModel
	viewers   map[int64]int64
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

type postStore struct {
	db                           *sqlx.DB
	generation                   string
	file                         *os.File
	capacity                     int64
	life                         sync.RWMutex
	closed                       bool
	syncFile                     func() error
	mu                           sync.Mutex
	err                          error
	streams                      map[int64]*postStream
	rows, memory                 int64
	slots                        chan struct{}
	requests                     chan postRequest
	writers                      sync.WaitGroup
	durableSeq, publishedSeq     atomic.Uint64
	durableBytes, publishedBytes atomic.Int64
	syncs, syncNanos, records    atomic.Uint64
	singleRecordSyncs            atomic.Uint64
	nextComment, nextReaction    int64
	walMu                        sync.Mutex
	pending                      []postBatch
	syncWake                     chan struct{}
	writtenSeq                   uint64
	writtenBytes                 int64
	nextWord, nextReport         int64
	iconMu                       sync.Mutex
	icons                        map[string]*walIcon
	iconRequests                 chan *iconWrite
	iconPendingBytes             int64
	iconReuses, iconUploads      atomic.Uint64
	iconBytes, iconChunks        atomic.Uint64
	dataMu                       sync.RWMutex
	commentsByID                 map[int64]LivecommentModel
	userNames                    map[int64]string
	owners                       map[int64]int64
	streamTotals, userTotals     map[int64]*postTotals
	totalTip                     int64
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
	state := &postStream{gate: make(chan struct{}, 1), viewers: make(map[int64]int64)}
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
	if s.iconRequests != nil {
		close(s.iconRequests)
	}
	s.writers.Wait()
	s.mu.Lock()
	s.closed = true
	err := s.err
	s.mu.Unlock()
	if e := s.file.Close(); err == nil {
		err = e
	}
	return err
}

func (s *postStore) metrics() map[string]any {
	s.mu.Lock()
	rows, memory, failed := s.rows, s.memory, s.err != nil
	s.mu.Unlock()
	s.walMu.Lock()
	pending := s.writtenBytes - s.durableBytes.Load()
	s.walMu.Unlock()
	return map[string]any{"generation": s.generation, "durable_seq": s.durableSeq.Load(), "published_seq": s.publishedSeq.Load(), "wal_bytes": s.durableBytes.Load(), "pending_bytes": pending, "rows": rows, "model_budget_bytes": memory, "records": s.records.Load(), "syncs": s.syncs.Load(), "sync_ns": s.syncNanos.Load(), "single_record_syncs": s.singleRecordSyncs.Load(), "failed": failed, "icon_reuses": s.iconReuses.Load(), "icon_uploads": s.iconUploads.Load(), "icon_bytes": s.iconBytes.Load(), "icon_chunks": s.iconChunks.Load()}
}

func postDirectory() string {
	if dir := os.Getenv("ISUPIPE_WAL_DIR"); dir != "" {
		return dir
	}
	return "/var/lib/isupipe/wal"
}

func ensurePostStream(id, owner int64) error {
	s := currentMutations.Load()
	if s == nil {
		return postsUnavailable("post store is not initialized")
	}
	if _, err := s.stream(id); err != nil {
		return err
	}
	s.addStream(id, owner)
	return nil
}
