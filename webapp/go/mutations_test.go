package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

func checkMutationBatches(t *testing.T, db *sqlx.DB, e *echo.Echo, store *sessions.CookieStore) {
	ctx := context.Background()
	writer := currentMutations.Load()
	t.Run("partial_failure_and_commit_failure", func(t *testing.T) {
		if _, err := db.Exec("CREATE TABLE batch_test (id BIGINT PRIMARY KEY)"); err != nil {
			t.Fatal(err)
		}
		request := func(id int64, reject bool) mutationRequest {
			return mutationRequest{ctx: ctx, done: make(chan error, 1), apply: func(work context.Context, tx *sqlx.Tx) error {
				if _, err := tx.ExecContext(work, "INSERT INTO batch_test VALUES (?)", id); err != nil {
					return err
				}
				if reject {
					return echo.NewHTTPError(400, "request rejected")
				}
				return nil
			}}
		}
		batch := []mutationRequest{request(1, false), request(2, true), request(3, false)}
		before := writer.commits.Load()
		writer.write(batch)
		for i, r := range batch {
			err := <-r.done
			if (err != nil) != (i == 1) {
				t.Fatalf("request %d: %v", i, err)
			}
		}
		var ids []int64
		if err := db.Select(&ids, "SELECT id FROM batch_test ORDER BY id"); err != nil || fmt.Sprint(ids) != "[1 3]" || writer.commits.Load() != before+1 {
			t.Fatalf("partial batch: %v %v", ids, err)
		}
		failed := request(4, false)
		failed.apply = func(work context.Context, tx *sqlx.Tx) error {
			if _, err := tx.ExecContext(work, "INSERT INTO batch_test VALUES (4)"); err != nil {
				return err
			}
			var id int64
			if err := tx.GetContext(work, &id, "SELECT CONNECTION_ID()"); err != nil {
				return err
			}
			_, err := db.Exec(fmt.Sprintf("KILL CONNECTION %d", id))
			return err
		}
		writer.write([]mutationRequest{failed})
		if err := <-failed.done; err == nil {
			t.Fatal("failed COMMIT acknowledged")
		}
		var count int
		if err := db.Get(&count, "SELECT COUNT(*) FROM batch_test WHERE id=4"); err != nil || count != 0 {
			t.Fatalf("failed COMMIT retained data: %d %v", count, err)
		}
	})
	t.Run("queue_batches_and_cancellation", func(t *testing.T) {
		gate := make(chan struct{})
		started := make(chan struct{})
		cancelled, cancel := context.WithCancel(ctx)
		firstDone := make(chan error, 1)
		go func() {
			firstDone <- writer.submit(cancelled, 1, func(work context.Context, tx *sqlx.Tx) error {
				close(started)
				<-gate
				_, err := tx.ExecContext(work, "INSERT INTO batch_test VALUES (10)")
				return err
			})
		}()
		<-started
		before := writer.commits.Load()
		var wg sync.WaitGroup
		for i := int64(11); i < 27; i++ {
			wg.Add(1)
			go func(id int64) {
				defer wg.Done()
				if err := writer.submit(ctx, 1, func(work context.Context, tx *sqlx.Tx) error {
					_, err := tx.ExecContext(work, "INSERT INTO batch_test VALUES (?)", id)
					return err
				}); err != nil {
					t.Errorf("unrelated request: %v", err)
				}
			}(i)
		}
		deadline := time.Now().Add(time.Second)
		for len(writer.queues[1]) < 16 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		cancel()
		close(gate)
		<-firstDone
		wg.Wait()
		var count int
		if err := db.Get(&count, "SELECT COUNT(*) FROM batch_test WHERE id>=10"); err != nil || count != 17 {
			t.Fatalf("client cancellation corrupted other requests: %d %v", count, err)
		}
		commits := writer.commits.Load() - before
		if commits >= 17 {
			t.Fatalf("queue did not batch: %d commits", commits)
		}
		t.Logf("17 mutations committed in %d transactions", commits)
	})
	t.Run("report_survives_moderation_and_viewer_counts", func(t *testing.T) {
		if _, err := db.Exec("DELETE FROM ng_words;DELETE FROM livecomments;DELETE FROM livecomment_reports"); err != nil {
			t.Fatal(err)
		}
		rec := improvementRequest(t, e, store, http.MethodPost, "/api/livestream/1/livecomment", `{"comment":"preserved-report","tip":10}`, 1)
		if rec.Code != 201 {
			t.Fatalf("post: %s", rec.Body)
		}
		var posted Livecomment
		if err := json.Unmarshal(rec.Body.Bytes(), &posted); err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := db.Get(&id, "SELECT id FROM livecomments WHERE comment='preserved-report'"); err != nil {
			t.Fatal(err)
		}
		path := fmt.Sprintf("/api/livestream/1/livecomment/%d/report", id)
		if rec := improvementRequest(t, e, store, http.MethodPost, path, "", 2); rec.Code != 201 {
			t.Fatalf("report: %s", rec.Body)
		}
		if rec := improvementRequest(t, e, store, http.MethodPost, "/api/livestream/1/moderate", `{"ng_word":"preserved"}`, 1); rec.Code != 201 {
			t.Fatalf("moderate: %s", rec.Body)
		}
		rec = improvementRequest(t, e, store, http.MethodGet, "/api/livestream/1/report", "", 1)
		if rec.Code != 200 {
			t.Fatalf("deleted report returned %d: %s", rec.Code, rec.Body)
		}
		var reports []LivecommentReport
		if err := json.Unmarshal(rec.Body.Bytes(), &reports); err != nil || len(reports) != 1 {
			t.Fatalf("reports: %s %v", rec.Body, err)
		}
		got := reports[0].Livecomment
		if got.ID != posted.ID || got.Comment != posted.Comment || got.Tip != posted.Tip || got.CreatedAt != posted.CreatedAt || got.User.ID != posted.User.ID || got.Livestream.ID != posted.Livestream.ID {
			t.Fatalf("report lost comment snapshot: %+v, want %+v", got, posted)
		}
		if rec := improvementRequest(t, e, store, http.MethodPost, path, "", 2); rec.Code != 404 {
			t.Fatalf("deleted comment accepted report: %d", rec.Code)
		}
		if _, err := db.Exec("DELETE FROM livestream_viewers_history WHERE livestream_id=1"); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if rec := improvementRequest(t, e, store, http.MethodPost, "/api/livestream/1/enter", "", 2); rec.Code != 200 {
				t.Fatal(rec.Body)
			}
		}
		if rec := improvementRequest(t, e, store, http.MethodDelete, "/api/livestream/1/exit", "", 2); rec.Code != 200 {
			t.Fatal(rec.Body)
		}
		var count int
		if err := db.Get(&count, "SELECT COUNT(*) FROM livestream_viewers_history WHERE livestream_id=1"); err != nil || count != 0 {
			t.Fatalf("exit did not remove duplicated enters: %d %v", count, err)
		}
	})
	t.Run("four_workers_order_and_drain", func(t *testing.T) {
		if _, err := db.Exec("CREATE TABLE stream_batch_test (id BIGINT PRIMARY KEY, step INT NOT NULL)"); err != nil {
			t.Fatal(err)
		}
		w := newMutationWriter(db)
		gate := make(chan struct{})
		var release sync.Once
		defer w.close()
		defer release.Do(func() { close(gate) })
		started := make(chan int64, mutationWriterCount)
		results := make(chan error, 3*mutationWriterCount)
		for stream := int64(1); stream <= mutationWriterCount; stream++ {
			go func(stream int64) {
				results <- w.submit(ctx, stream, func(work context.Context, tx *sqlx.Tx) error {
					if _, err := tx.ExecContext(work, "INSERT INTO stream_batch_test VALUES (?, 0)", stream); err != nil {
						return err
					}
					started <- stream
					<-gate
					return nil
				})
			}(stream)
		}
		for i := 0; i < mutationWriterCount; i++ {
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("four streams did not execute concurrently")
			}
		}
		for step := 1; step <= 2; step++ {
			for stream := int64(1); stream <= mutationWriterCount; stream++ {
				go func(stream int64, step int) {
					results <- w.submit(ctx, stream, func(work context.Context, tx *sqlx.Tx) error {
						var previous int
						if err := tx.GetContext(work, &previous, "SELECT step FROM stream_batch_test WHERE id=?", stream); err != nil {
							return err
						}
						if previous != step-1 {
							return fmt.Errorf("stream %d step %d followed %d", stream, step, previous)
						}
						_, err := tx.ExecContext(work, "UPDATE stream_batch_test SET step=? WHERE id=?", step, stream)
						return err
					})
				}(stream, step)
				waitMutationQueue(t, w, stream, step)
			}
		}
		stopped := make(chan struct{})
		go func() { w.close(); close(stopped) }()
		deadline := time.Now().Add(time.Second)
		for {
			w.mu.RLock()
			closed := w.closed
			w.mu.RUnlock()
			if closed {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("writer did not close its queues")
			}
			time.Sleep(time.Millisecond)
		}
		select {
		case <-stopped:
			t.Fatal("close returned with active transactions")
		default:
		}
		release.Do(func() { close(gate) })
		for i := 0; i < 3*mutationWriterCount; i++ {
			select {
			case err := <-results:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("accepted mutation was not drained")
			}
		}
		<-stopped
		var complete int
		if err := db.Get(&complete, "SELECT COUNT(*) FROM stream_batch_test WHERE step=2"); err != nil || complete != mutationWriterCount {
			t.Fatalf("committed stream order: %d %v", complete, err)
		}
		if err := w.submit(ctx, 1, func(context.Context, *sqlx.Tx) error { t.Error("closed writer executed mutation"); return nil }); err == nil {
			t.Fatal("closed writer accepted mutation")
		}
		t.Log("four streams executed concurrently; each stream preserved order and drained on close")
	})
	t.Run("moderation_order_and_cross_stream_progress", func(t *testing.T) {
		if _, err := db.Exec("DELETE FROM livecomments; DELETE FROM ng_words"); err != nil {
			t.Fatal(err)
		}
		gate := make(chan struct{})
		var release sync.Once
		defer release.Do(func() { close(gate) })
		started := make(chan struct{})
		first := make(chan error, 1)
		go func() {
			first <- writer.submit(ctx, 1, func(work context.Context, tx *sqlx.Tx) error {
				// An empty indexed DELETE must not gap-lock unrelated streams.
				if _, err := tx.ExecContext(work, "DELETE FROM livecomments WHERE livestream_id=1"); err != nil {
					return err
				}
				close(started)
				<-gate
				return nil
			})
		}()
		select {
		case <-started:
		case err := <-first:
			t.Fatal(err)
		case <-time.After(time.Second):
			t.Fatal("first stream did not start")
		}
		other := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			other <- improvementRequest(t, e, store, http.MethodPost, "/api/livestream/2/livecomment", `{"comment":"independent stream","tip":3}`, 2)
		}()
		select {
		case rec := <-other:
			if rec.Code != 201 {
				t.Fatalf("independent stream: %d %s", rec.Code, rec.Body)
			}
		case <-time.After(time.Second):
			t.Fatal("moderation blocked another stream's INSERT")
		}
		moderated := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			moderated <- improvementRequest(t, e, store, http.MethodPost, "/api/livestream/1/moderate", `{"ng_word":"stream-order"}`, 1)
		}()
		waitMutationQueue(t, writer, 1, 1)
		commented := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			commented <- improvementRequest(t, e, store, http.MethodPost, "/api/livestream/1/livecomment", `{"comment":"stream-order","tip":10}`, 2)
		}()
		waitMutationQueue(t, writer, 1, 2)
		release.Do(func() { close(gate) })
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if rec := <-moderated; rec.Code != 201 {
			t.Fatalf("moderate: %d %s", rec.Code, rec.Body)
		}
		if rec := <-commented; rec.Code != 400 {
			t.Fatalf("NG registration was reordered: %d %s", rec.Code, rec.Body)
		}
	})
}

func waitMutationQueue(t *testing.T, w *mutationWriter, stream int64, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(w.queues[uint64(stream)%mutationWriterCount]) < count {
		if time.Now().After(deadline) {
			t.Fatal("mutation did not enter its stream queue")
		}
		time.Sleep(time.Millisecond)
	}
}
