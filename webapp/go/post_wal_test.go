package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

func TestPostFrames(t *testing.T) {
	records := []postRecord{{Seq: 1, Kind: "comment", ID: 1, UserID: 2, StreamID: 3, Text: "quote'\\😀", Tip: 7, CreatedAt: 123}, {Seq: 2, Kind: "reaction", ID: 1, Text: "emoji"}}
	frame, err := encodePostFrame(records)
	if err != nil {
		t.Fatal(err)
	}
	got, size, err := readPostFrame(bytes.NewReader(frame), 0)
	if err != nil || size != int64(len(frame)) || fmt.Sprint(got) != fmt.Sprint(records) {
		t.Fatalf("roundtrip: %v %d %v", got, size, err)
	}
	for _, cut := range []int{1, 7, 8, len(frame) - 1} {
		if _, _, err := readPostFrame(bytes.NewReader(frame[:cut]), 0); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("partial frame at %d: %v", cut, err)
		}
	}
	bad := append([]byte{}, frame...)
	bad[len(bad)-1] ^= 1
	if _, _, err := readPostFrame(bytes.NewReader(bad), 0); err == nil {
		t.Fatal("corrupted frame accepted")
	}
	if _, _, err := readPostFrame(bytes.NewReader(frame), 1); err == nil {
		t.Fatal("sequence gap accepted")
	}
}

func writerTestStore(t *testing.T) *postStore {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "posts")
	if err != nil {
		t.Fatal(err)
	}
	if err := writePostBytes(file, []byte(postMagic+"00000000-0000-0000-0000-000000000000")); err != nil {
		t.Fatal(err)
	}
	s := &postStore{file: file, syncFile: file.Sync, changed: make(chan struct{}), streams: make(map[int64]*postStream), slots: make(chan struct{}, 256), requests: make(chan postRequest, 256), nextComment: 1, nextReaction: 1}
	s.durableBytes.Store(int64(postHeaderSize))
	s.appliedBytes.Store(int64(postHeaderSize))
	t.Cleanup(func() { close(s.requests); s.writers.Wait(); file.Close() })
	return s
}

func TestPostSyncPublicationAndCancellation(t *testing.T) {
	s := writerTestStore(t)
	started, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(release) }) })
	s.syncFile = func() error { close(started); <-release; return s.file.Sync() }
	s.writers.Add(1)
	go s.writePosts()
	ctx, cancel := context.WithCancel(context.Background())
	model := &ReactionModel{UserID: 1, LivestreamID: 1, EmojiName: "first"}
	done := make(chan error, 1)
	go func() { done <- s.submit(ctx, 1, &reactionMutation{model: model}) }()
	<-started
	cancel()
	select {
	case err := <-done:
		t.Fatalf("returned before Sync: %v", err)
	default:
	}
	if s.durableSeq.Load() != 0 {
		t.Fatal("unsynchronized frame published")
	}
	readCtx, stop := context.WithTimeout(context.Background(), 5*time.Millisecond)
	_, _, err := s.timeline(readCtx, 1, -1, false)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read bypassed pending publication: %v", err)
	}
	unblock.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_, rows, err := s.timeline(context.Background(), 1, -1, false)
	if err != nil || len(rows) != 1 || rows[0].ID != model.ID || model.ID <= 0 {
		t.Fatalf("durable publication: %+v %v", rows, err)
	}
}

func TestPostSyncFailureAndGrouping(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		s := writerTestStore(t)
		s.syncFile = func() error { return errors.New("injected Sync failure") }
		s.writers.Add(1)
		go s.writePosts()
		model := &ReactionModel{LivestreamID: 1, EmojiName: "never-acknowledged"}
		if err := s.submit(context.Background(), 1, &reactionMutation{model: model}); err == nil || model.ID != 0 {
			t.Fatal("failed Sync acknowledged")
		}
		if _, _, err := s.timeline(context.Background(), 1, -1, false); err == nil {
			t.Fatal("failed engine served timeline")
		}
	})
	t.Run("grouping", func(t *testing.T) {
		s := writerTestStore(t)
		batch := make([]postRequest, 16)
		for i := range batch {
			batch[i] = postRequest{record: postRecord{Kind: "reaction", UserID: 1, StreamID: int64(i + 1), Text: "grouped"}, done: make(chan postResult, 1)}
		}
		s.writePostBatch(batch)
		for i, r := range batch {
			result := <-r.done
			if result.err != nil || result.record.ID != int64(i+1) {
				t.Fatalf("group result: %+v", result)
			}
		}
		if s.syncs.Load() != 1 || s.records.Load() != 16 {
			t.Fatal("independent streams did not share Sync")
		}
	})
}

func resetTestPosts(t *testing.T, db *sqlx.DB) string {
	t.Helper()
	if old := currentMutations.Swap(nil); old != nil {
		_ = old.close()
	}
	if _, err := db.Exec("UPDATE post_wal_state SET ready=FALSE WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := createPostGeneration(context.Background(), db, dir); err != nil {
		t.Fatal(err)
	}
	s, err := openPostStore(context.Background(), db, dir)
	if err != nil {
		t.Fatal(err)
	}
	currentMutations.Store(s)
	return dir
}

func checkDurablePosts(t *testing.T, db *sqlx.DB, e *echo.Echo, cookies *sessions.CookieStore) {
	ctx := context.Background()
	clearRows := func() {
		if old := currentMutations.Swap(nil); old != nil {
			_ = old.close()
		}
		if _, err := db.Exec("DELETE FROM ng_words; DELETE FROM livecomments; DELETE FROM reactions; DELETE FROM livecomment_reports"); err != nil {
			t.Fatal(err)
		}
	}
	e.GET("/api/payment", GetPaymentResult)
	t.Run("projection_atomicity_and_lost_commit_reply", func(t *testing.T) {
		clearRows()
		resetTestPosts(t, db)
		s := currentMutations.Load()
		records := []postRecord{{Seq: 1, Kind: "comment", ID: 1000001, UserID: 2, StreamID: 1, Text: "atomic", Tip: 5}, {Seq: 2, Kind: "reaction", ID: 1000001, UserID: 2, StreamID: 1, Text: "atomic"}}
		if _, err := db.Exec("CREATE TRIGGER reject_atomic_projection BEFORE INSERT ON reactions FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected second-table failure'"); err != nil {
			t.Fatal(err)
		}
		if err := s.projectFrameBatch(ctx, records); err == nil {
			t.Fatal("partial projection succeeded")
		}
		if _, err := db.Exec("DROP TRIGGER reject_atomic_projection"); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.Get(&count, "SELECT COUNT(*) FROM livecomments WHERE id=1000001"); err != nil || count != 0 {
			t.Fatalf("partial row committed: %d %v", count, err)
		}
		state, err := readPostState(ctx, db)
		if err != nil || state.Applied != 0 {
			t.Fatalf("partial marker committed: %+v %v", state, err)
		}
		config, err := mysql.ParseDSN(os.Getenv("ISUCON_TEST_MYSQL_DSN"))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Get(&config.DBName, "SELECT DATABASE()"); err != nil {
			t.Fatal(err)
		}
		connector, err := mysql.NewConnector(config)
		if err != nil {
			t.Fatal(err)
		}
		faultDB := sqlx.NewDb(sql.OpenDB(commitReplyLostConnector{connector}), "mysql")
		defer faultDB.Close()
		s.db = faultDB
		if err := s.projectFrameBatch(ctx, records); err != nil {
			t.Fatalf("committed marker was not recognized: %v", err)
		}
		s.db = db
		state, err = readPostState(ctx, db)
		if err != nil || state.Applied != 2 {
			t.Fatalf("lost reply marker: %+v %v", state, err)
		}
	})
	t.Run("concurrent_posts_reads_moderation_and_restart", func(t *testing.T) {
		clearRows()
		dir := resetTestPosts(t, db)
		s := currentMutations.Load()
		var wg sync.WaitGroup
		var idsMu sync.Mutex
		ids := map[int64]bool{}
		for stream := int64(1); stream <= 3; stream++ {
			wg.Add(1)
			go func(stream int64) {
				defer wg.Done()
				for i := 0; i < 8; i++ {
					model := &LivecommentModel{UserID: 2, LivestreamID: stream, Comment: fmt.Sprintf("post-%d-%d", stream, i), Tip: 7, CreatedAt: 123}
					if err := s.submit(ctx, stream, &commentMutation{model: model, ownerID: 1}); err != nil {
						t.Error(err)
						return
					}
					idsMu.Lock()
					if ids[model.ID] {
						t.Error("duplicate post ID")
					}
					ids[model.ID] = true
					idsMu.Unlock()
					rows, _, err := s.timeline(ctx, stream, 1, true)
					if err != nil || len(rows) != 1 || rows[0].ID != model.ID {
						t.Errorf("immediate read: %+v %v", rows, err)
					}
				}
			}(stream)
		}
		wg.Wait()
		if err := flushPosts(ctx); err != nil {
			t.Fatal(err)
		}
		var total int
		if err := db.Get(&total, "SELECT COUNT(*) FROM livecomments"); err != nil || total != 24 {
			t.Fatalf("projected posts: %d %v", total, err)
		}
		rec := improvementRequest(t, e, cookies, http.MethodPost, "/api/livestream/1/livecomment", `{"comment":"moderation-target","tip":11}`, 2)
		if rec.Code != 201 {
			t.Fatal(rec.Body)
		}
		var posted Livecomment
		if err := json.Unmarshal(rec.Body.Bytes(), &posted); err != nil {
			t.Fatal(err)
		}
		path := fmt.Sprintf("/api/livestream/1/livecomment/%d/report", posted.ID)
		if rec := improvementRequest(t, e, cookies, http.MethodPost, path, "", 2); rec.Code != 201 {
			t.Fatal(rec.Body)
		}
		if rec := improvementRequest(t, e, cookies, http.MethodPost, "/api/livestream/1/moderate", `{"ng_word":"moderation-target"}`, 1); rec.Code != 201 {
			t.Fatal(rec.Body)
		}
		if rec := improvementRequest(t, e, cookies, http.MethodPost, "/api/livestream/1/livecomment", `{"comment":"MODERATION-TARGET","tip":11}`, 2); rec.Code != 400 {
			t.Fatalf("NG predicate: %d %s", rec.Code, rec.Body)
		}
		if rec := improvementRequest(t, e, cookies, http.MethodPost, path, "", 2); rec.Code != 404 {
			t.Fatalf("deleted report: %d", rec.Code)
		}
		read := improvementRequest(t, e, cookies, http.MethodGet, "/api/livestream/1/livecomment", "", 1)
		if read.Code != 200 || strings.Contains(read.Body.String(), "moderation-target") {
			t.Fatalf("deleted memory row: %s", read.Body)
		}
		payment := improvementRequest(t, e, cookies, http.MethodGet, "/api/payment", "", 1)
		var amount PaymentResult
		if err := json.Unmarshal(payment.Body.Bytes(), &amount); err != nil || payment.Code != 200 || amount.TotalTip != 168 {
			t.Fatalf("payment after deletion: %s %v", payment.Body, err)
		}
		if err := s.close(); err != nil {
			t.Fatal(err)
		}
		// A partial trailing frame cannot resurrect a previously projected deletion.
		file, err := os.OpenFile(filepath.Join(dir, s.generation+".wal"), os.O_APPEND|os.O_WRONLY, 0640)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte{1, 2, 3}); err != nil {
			t.Fatal(err)
		}
		file.Close()
		recovered, err := openPostStore(ctx, db, dir)
		if err != nil {
			t.Fatal(err)
		}
		currentMutations.Store(recovered)
		read = improvementRequest(t, e, cookies, http.MethodGet, "/api/livestream/1/livecomment", "", 1)
		if read.Code != 200 || strings.Contains(read.Body.String(), "moderation-target") {
			t.Fatalf("deletion resurrected: %s", read.Body)
		}
		next := &LivecommentModel{UserID: 2, LivestreamID: 1, Comment: "after-restart", Tip: 3}
		if err := recovered.submit(ctx, 1, &commentMutation{model: next, ownerID: 1}); err != nil || next.ID <= posted.ID {
			t.Fatalf("ID reused after deletion/recovery: %d %v", next.ID, err)
		}
		reports := improvementRequest(t, e, cookies, http.MethodGet, "/api/livestream/1/report", "", 1)
		if reports.Code != 200 || !strings.Contains(reports.Body.String(), "moderation-target") {
			t.Fatal("report snapshot lost")
		}
	})
	t.Run("unprojected_durable_frame", func(t *testing.T) {
		clearRows()
		dir := resetTestPosts(t, db)
		if _, err := db.Exec("CREATE TRIGGER reject_projection BEFORE INSERT ON reactions FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected projection failure'"); err != nil {
			t.Fatal(err)
		}
		s := currentMutations.Load()
		model := &ReactionModel{UserID: 2, LivestreamID: 1, EmojiName: "durable-unprojected"}
		if err := s.submit(ctx, 1, &reactionMutation{model: model}); err != nil {
			t.Fatal(err)
		}
		if err := flushPosts(ctx); err == nil {
			t.Fatal("failed projector reported success")
		}
		if err := s.close(); err == nil {
			t.Fatal("failed projector closed successfully")
		}
		if _, err := db.Exec("DROP TRIGGER reject_projection"); err != nil {
			t.Fatal(err)
		}
		recovered, err := openPostStore(ctx, db, dir)
		if err != nil {
			t.Fatal(err)
		}
		currentMutations.Store(recovered)
		var row ReactionModel
		if err := db.Get(&row, "SELECT * FROM reactions WHERE id=?", model.ID); err != nil || row != *model {
			t.Fatalf("unprojected recovery: %+v %v", row, err)
		}
		if err := recovered.close(); err != nil {
			t.Fatal(err)
		}
		recovered, err = openPostStore(ctx, db, dir)
		if err != nil {
			t.Fatal(err)
		}
		currentMutations.Store(recovered)
		var count int
		if err := db.Get(&count, "SELECT COUNT(*) FROM reactions WHERE id=?", model.ID); err != nil || count != 1 {
			t.Fatalf("replay duplication: %d %v", count, err)
		}
	})
	t.Run("unready_generation", func(t *testing.T) {
		s := currentMutations.Swap(nil)
		dir := filepath.Dir(s.file.Name())
		if err := s.close(); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("UPDATE post_wal_state SET ready=FALSE WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		if recovered, err := openPostStore(ctx, db, dir); err != nil || recovered != nil {
			t.Fatalf("incomplete initialization resumed: %v %v", recovered, err)
		}
		if _, err := db.Exec("DELETE FROM livecomments; DELETE FROM reactions"); err != nil {
			t.Fatal(err)
		}
		gen, err := createPostGeneration(ctx, db, dir)
		if err != nil {
			t.Fatal(err)
		}
		if gen == s.generation {
			t.Fatal("initialization reused generation")
		}
		recovered, err := openPostStore(ctx, db, dir)
		if err != nil {
			t.Fatal(err)
		}
		currentMutations.Store(recovered)
		comments, reactions, err := recovered.timeline(ctx, 1, -1, true)
		if err != nil || len(comments) != 0 || len(reactions) != 0 {
			t.Fatalf("old generation replayed: %+v %+v %v", comments, reactions, err)
		}
		_, reactions, err = recovered.timeline(ctx, 1, -1, false)
		if err != nil || len(reactions) != 0 {
			t.Fatalf("old reaction replayed: %+v %v", reactions, err)
		}
		if err := removeOldPostGenerations(dir, gen); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(s.file.Name()); !os.IsNotExist(err) {
			t.Fatalf("old generation retained: %v", err)
		}
	})
}

// The real database commits, but the client receives a broken-connection error.
type commitReplyLostConnector struct{ driver.Connector }

func (c commitReplyLostConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return commitReplyLostConn{conn}, nil
}

type commitReplyLostConn struct{ driver.Conn }

func (c commitReplyLostConn) Begin() (driver.Tx, error) {
	tx, err := c.Conn.Begin()
	if err != nil {
		return nil, err
	}
	return commitReplyLostTx{tx}, nil
}
func (c commitReplyLostConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return commitReplyLostTx{tx}, nil
}

type commitReplyLostTx struct{ driver.Tx }

func (t commitReplyLostTx) Commit() error {
	if err := t.Tx.Commit(); err != nil {
		return err
	}
	return driver.ErrBadConn
}
