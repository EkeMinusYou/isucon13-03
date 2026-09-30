package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
			firstDone <- writer.submit(cancelled, func(work context.Context, tx *sqlx.Tx) error {
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
				if err := writer.submit(ctx, func(work context.Context, tx *sqlx.Tx) error {
					_, err := tx.ExecContext(work, "INSERT INTO batch_test VALUES (?)", id)
					return err
				}); err != nil {
					t.Errorf("unrelated request: %v", err)
				}
			}(i)
		}
		deadline := time.Now().Add(time.Second)
		for len(writer.queue) < 16 && time.Now().Before(deadline) {
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
}
