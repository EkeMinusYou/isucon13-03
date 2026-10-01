package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

func testIconJournal(t *testing.T) *postStore {
	t.Helper()
	s := writerTestStore(t)
	s.writers.Add(1)
	go s.writePosts()
	return s
}

func TestIconWALAndConditionalGET(t *testing.T) {
	s := &iconStore{journal: testIconJournal(t), byName: make(map[string]*iconEntry), byID: make(map[int64]*iconEntry)}
	image := []byte("persisted-icon")
	hash, err := s.persist(image)
	if err != nil {
		t.Fatal(err)
	}
	// Reusing a durable hash must not add a frame or another Sync.
	before := s.journal.syncs.Load()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := s.persist(image); err != nil || got != hash {
				t.Errorf("persist identical icon: hash=%s err=%v", got, err)
			}
		}()
	}
	wg.Wait()
	reader, err := s.journal.iconReader(hash)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(reader)
	if err != nil || string(stored) != string(image) {
		t.Fatalf("persisted file: image=%q err=%v", stored, err)
	}
	if s.journal.syncs.Load() != before {
		t.Fatal("duplicate image required another Sync")
	}
	previous := currentIcons
	currentIcons = s
	defer func() { currentIcons = previous }()
	s.addUser(1, "alpha", hash)
	e := echo.New()
	e.GET("/api/user/:username/icon", getIconHandler)
	for _, tc := range []struct {
		name, etag string
		status     int
	}{
		{"alpha", "", 200}, {"alpha", "\"" + hash + "\"", 304},
		{"alpha", "\"stale\"", 200}, {"alpha", "W/\"" + hash + "\"", 304},
		{"alpha", "\"stale\", \"" + hash + "\"", 304}, {"missing", "", 404},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/user/"+tc.name+"/icon", nil)
		req.Header.Set("If-None-Match", tc.etag)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("conditional GET: etag=%s status=%d want=%d", tc.etag, rec.Code, tc.status)
		}
		if tc.status == 200 && (rec.Body.String() != string(image) || rec.Header().Get("X-Accel-Redirect") != "" || rec.Header().Get("ETag") != "\""+hash+"\"") {
			t.Fatal("body and ETag must match the exact immutable generation")
		}
		if tc.status == 304 && (rec.Body.Len() != 0 || rec.Header().Get("X-Accel-Redirect") != "") {
			t.Fatal("304 must not transfer a file")
		}
	}
}

func checkIconsMySQL(t *testing.T, db *sqlx.DB, e *echo.Echo, store *struct{}) {
	requests := make([]*http.Request, 0, 4)
	for i, id := range []int64{1, 1, 2, 3} {
		body, _ := json.Marshal(PostIconRequest{Image: []byte(fmt.Sprintf("concurrent-%d", i))})
		requests = append(requests, improvementHTTPRequest(t, store, http.MethodPost, "/api/icon", string(body), id))
	}
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, len(requests))
	for _, req := range requests {
		go func(req *http.Request) {
			<-start
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			results <- rec
		}(req)
	}
	close(start)
	ids := make(map[int64]bool)
	for range requests {
		select {
		case rec := <-results:
			var response PostIconResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != 201 || response.ID == 0 || ids[response.ID] {
				t.Fatalf("concurrent upload: status=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
			}
			ids[response.ID] = true
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent icon uploads timed out")
		}
	}
	journal := currentIcons.journal
	// Rebuild the complete in-memory index from durable state, as on app restart.
	var err error
	if err = journal.close(); err != nil {
		t.Fatal(err)
	}
	journal, err = openPostStore(context.Background(), db, filepath.Dir(journal.file.Name()))
	if err != nil {
		t.Fatal(err)
	}
	currentMutations.Store(journal)
	currentIcons, err = newIconStore(context.Background(), journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := reloadReferences(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta", "empty"} {
		var hash string
		if err := db.Get(&hash, "SELECT icon_hash FROM users WHERE name=?", name); err != nil {
			t.Fatal(err)
		}
		rec := improvementRequest(t, e, store, http.MethodGet, "/api/user/"+name+"/icon", "", 1)
		if rec.Code != 200 || fmt.Sprintf("%x", sha256.Sum256(rec.Body.Bytes())) != hash || rec.Header().Get("ETag") != "\""+hash+"\"" {
			t.Fatalf("restored index disagrees with database: name=%s status=%d headers=%v", name, rec.Code, rec.Header())
		}
		reader, err := journal.iconReader(hash)
		if err != nil {
			t.Fatal(err)
		}
		image, err := io.ReadAll(reader)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(image)) != hash {
			t.Fatalf("restored hash disagrees with file: name=%s err=%v", name, err)
		}
	}
	// An empty upload has its own hash and must not become the fallback image.
	body, _ := json.Marshal(PostIconRequest{Image: []byte{}})
	if rec := improvementRequest(t, e, store, http.MethodPost, "/api/icon", string(body), 2); rec.Code != 201 {
		t.Fatalf("empty upload: status=%d body=%s", rec.Code, rec.Body.String())
	}
	emptyHash := fmt.Sprintf("%x", sha256.Sum256(nil))
	if rec := improvementRequest(t, e, store, http.MethodGet, "/api/user/beta/icon", "", 1); rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("ETag") != "\""+emptyHash+"\"" {
		t.Fatal("empty upload was replaced with fallback")
	}
	// Recover fragmented images and ignore an image whose last chunk never arrived.
	large := bytes.Repeat([]byte("fragmented-icon"), 12000)
	body, _ = json.Marshal(PostIconRequest{Image: large})
	if rec := improvementRequest(t, e, store, http.MethodPost, "/api/icon", string(body), 1); rec.Code != 201 {
		t.Fatalf("fragmented upload: %s", rec.Body.String())
	}
	orphan := bytes.Repeat([]byte("unfinished"), 10000)
	orphanHash := fmt.Sprintf("%x", sha256.Sum256(orphan))
	request := postRequest{record: postRecord{Kind: "icon", IconHash: orphanHash, IconTotal: int64(len(orphan)), IconSize: iconChunkSize, iconData: orphan[:iconChunkSize]}, done: make(chan postResult, 1)}
	journal.requests <- request
	if result := <-request.done; result.err != nil {
		t.Fatal(result.err)
	}
	for restart := 0; restart < 2; restart++ {
		if err := journal.close(); err != nil {
			t.Fatal(err)
		}
		journal, err = openPostStore(context.Background(), db, filepath.Dir(journal.file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		currentMutations.Store(journal)
		currentIcons, err = newIconStore(context.Background(), journal)
		if err != nil {
			t.Fatal(err)
		}
		rec := improvementRequest(t, e, store, http.MethodGet, "/api/user/alpha/icon", "", 1)
		if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), large) {
			t.Fatal("fragmented icon lost after restart")
		}
		if restart == 0 {
			if journal.hasIcon(orphanHash) {
				t.Fatal("unfinished image was recovered as durable")
			}
			if key, err := journal.persistIcon(context.Background(), orphan); err != nil || key != orphanHash {
				t.Fatalf("image retry after partial replay: %s %v", key, err)
			}
		} else {
			reader, err := journal.iconReader(orphanHash)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(reader)
			if err != nil || !bytes.Equal(got, orphan) {
				t.Fatal("retried image lost after second restart")
			}
		}
	}
	// Failed admission must leave the published database pointer unchanged.
	queue := journal.iconRequests
	journal.iconRequests = nil
	body, _ = json.Marshal(PostIconRequest{Image: []byte("must-not-publish")})
	rec := improvementRequest(t, e, store, http.MethodPost, "/api/icon", string(body), 2)
	journal.iconRequests = queue
	var hash string
	if err := db.Get(&hash, "SELECT icon_hash FROM users WHERE id=2"); err != nil || rec.Code != 500 || hash != emptyHash {
		t.Fatalf("failed persistence changed state: status=%d hash=%s err=%v", rec.Code, hash, err)
	}
	// Reproduce the icon part of official initialization and remove old generations.
	if _, err := db.Exec("UPDATE users SET icon_hash=''; TRUNCATE TABLE icons"); err != nil {
		t.Fatal(err)
	}
	resetTestPosts(t, db)
	currentIcons, err = newIconStore(context.Background(), currentMutations.Load())
	if err != nil {
		t.Fatal(err)
	}
	if err := reloadReferences(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := improvementRequest(t, e, store, http.MethodGet, "/api/user/alpha/icon", "", 1); rec.Code != 200 || fmt.Sprintf("%x", sha256.Sum256(rec.Body.Bytes())) != currentIcons.fallbackHash {
		t.Fatal("initialization did not restore fallback")
	}
	if len(currentIcons.journal.icons) != 1 || !currentIcons.journal.hasIcon(currentIcons.fallbackHash) {
		t.Fatal("initialization retained old image generation")
	}
	t.Log("concurrent uploads, durable reload, empty image, failed write and initialization passed")
}
