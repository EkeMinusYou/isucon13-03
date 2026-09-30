package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

const postDBTimeout = 5 * time.Second

func (s *postStore) projectFrameBatch(ctx context.Context, records []postRecord) error {
	work, cancel := context.WithTimeout(ctx, postDBTimeout)
	defer cancel()
	tx, err := s.db.BeginTxx(work, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertProjectedPosts(work, tx, records, true); err != nil {
		return err
	}
	if err := insertProjectedPosts(work, tx, records, false); err != nil {
		return err
	}
	last := records[len(records)-1].Seq
	result, err := tx.ExecContext(work, "UPDATE post_wal_state SET applied_seq=? WHERE id=1 AND generation=? AND ready=TRUE", last, s.generation)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("post projection generation changed")
	}
	if err := tx.Commit(); err != nil {
		// A successful transaction and its marker cannot be committed separately.
		check, stop := context.WithTimeout(context.Background(), postDBTimeout)
		defer stop()
		state, readErr := readPostState(check, s.db)
		if readErr == nil && state.Ready && state.Generation == s.generation && state.Applied == last {
			return nil
		}
		return fmt.Errorf("post projection COMMIT: %w (marker check: %v)", err, readErr)
	}
	return nil
}

func insertProjectedPosts(ctx context.Context, tx *sqlx.Tx, records []postRecord, comments bool) error {
	kind := "reaction"
	query := "INSERT INTO reactions (id,user_id,livestream_id,emoji_name,created_at) VALUES "
	values := "(?,?,?,?,?)"
	if comments {
		kind = "comment"
		query = "INSERT INTO livecomments (id,user_id,livestream_id,comment,tip,created_at) VALUES "
		values = "(?,?,?,?,?,?)"
	}
	var parts []string
	var args []interface{}
	for _, r := range records {
		if r.Kind != kind {
			continue
		}
		parts = append(parts, values)
		args = append(args, r.ID, r.UserID, r.StreamID, r.Text)
		if comments {
			args = append(args, r.Tip)
		}
		args = append(args, r.CreatedAt)
	}
	if len(parts) == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, query+strings.Join(parts, ","), args...)
	return err
}

func (s *postStore) projectPosts() {
	defer s.projectors.Done()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-s.wake:
		case <-s.stop:
			return
		}
		for s.appliedSeq.Load() < s.durableSeq.Load() {
			if err := s.check(); err != nil {
				return
			}
			var records []postRecord
			end := s.appliedBytes.Load()
			seq := s.appliedSeq.Load()
			available := s.durableBytes.Load()
			for end < available && len(records) <= 192 {
				frame, size, err := readPostFrame(s.reader, seq)
				if err != nil {
					s.fail(fmt.Errorf("read synchronized WAL: %w", err))
					return
				}
				records = append(records, frame...)
				seq = frame[len(frame)-1].Seq
				end += size
			}
			if len(records) == 0 {
				s.fail(fmt.Errorf("synchronized WAL sequence has no frame"))
				return
			}
			if err := s.projectFrameBatch(context.Background(), records); err != nil {
				s.fail(err)
				return
			}
			s.mu.Lock()
			s.appliedBytes.Store(end)
			s.appliedSeq.Store(seq)
			close(s.changed)
			s.changed = make(chan struct{})
			s.mu.Unlock()
		}
	}
}

func (s *postStore) flush(ctx context.Context, seq uint64) error {
	for {
		s.mu.Lock()
		if s.err != nil {
			err := postsUnavailable(s.err.Error())
			s.mu.Unlock()
			return err
		}
		if s.appliedSeq.Load() >= seq {
			s.mu.Unlock()
			return nil
		}
		if s.closed {
			s.mu.Unlock()
			return postsUnavailable("post store closed before flush")
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case s.wake <- struct{}{}:
		default:
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
