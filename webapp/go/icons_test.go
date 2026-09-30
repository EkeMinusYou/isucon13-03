package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

func TestIconFilesAndConditionalGET(t *testing.T) {
	s := &iconStore{dir: t.TempDir(), byName: make(map[string]*iconEntry), byID: make(map[int64]*iconEntry)}
	image := []byte("persisted-icon")
	hash, err := s.persist(image)
	if err != nil {
		t.Fatal(err)
	}
	// Concurrent publication of identical content must leave a complete readable file.
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
	stored, err := os.ReadFile(filepath.Join(s.dir, hash))
	if err != nil || string(stored) != string(image) {
		t.Fatalf("persisted file: image=%q err=%v", stored, err)
	}
	info, err := os.Stat(filepath.Join(s.dir, hash))
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("nginx-readable file permissions: info=%v err=%v", info, err)
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
		if tc.status == 200 && rec.Header().Get("X-Accel-Redirect") != "/_internal/icons/"+hash {
			t.Fatal("body must be delegated to the exact immutable generation")
		}
		if tc.status == 304 && (rec.Body.Len() != 0 || rec.Header().Get("X-Accel-Redirect") != "") {
			t.Fatal("304 must not transfer a file")
		}
	}
}

func checkIconsMySQL(t *testing.T, db *sqlx.DB, e *echo.Echo, store *sessions.CookieStore) {
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
	dir := currentIcons.dir
	// Rebuild the complete in-memory index from durable state, as on app restart.
	var err error
	currentIcons, err = newIconStore(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta", "empty"} {
		var hash string
		if err := db.Get(&hash, "SELECT icon_hash FROM users WHERE name=?", name); err != nil {
			t.Fatal(err)
		}
		rec := improvementRequest(t, e, store, http.MethodGet, "/api/user/"+name+"/icon", "", 1)
		if rec.Code != 200 || rec.Header().Get("X-Accel-Redirect") != "/_internal/icons/"+hash {
			t.Fatalf("restored index disagrees with database: name=%s status=%d headers=%v", name, rec.Code, rec.Header())
		}
		image, err := os.ReadFile(filepath.Join(dir, hash))
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
	if rec := improvementRequest(t, e, store, http.MethodGet, "/api/user/beta/icon", "", 1); rec.Header().Get("X-Accel-Redirect") != "/_internal/icons/"+emptyHash {
		t.Fatal("empty upload was replaced with fallback")
	}
	// A failed file write must leave the published database pointer unchanged.
	originalDir := currentIcons.dir
	currentIcons.dir = filepath.Join(dir, "missing-directory")
	body, _ = json.Marshal(PostIconRequest{Image: []byte("must-not-publish")})
	rec := improvementRequest(t, e, store, http.MethodPost, "/api/icon", string(body), 2)
	currentIcons.dir = originalDir
	var hash string
	if err := db.Get(&hash, "SELECT icon_hash FROM users WHERE id=2"); err != nil || rec.Code != 500 || hash != emptyHash {
		t.Fatalf("failed persistence changed state: status=%d hash=%s err=%v", rec.Code, hash, err)
	}
	// Reproduce the icon part of official initialization and remove old generations.
	if _, err := db.Exec("UPDATE users SET icon_hash=''; TRUNCATE TABLE icons"); err != nil {
		t.Fatal(err)
	}
	if err := currentIcons.reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := improvementRequest(t, e, store, http.MethodGet, "/api/user/alpha/icon", "", 1); rec.Header().Get("X-Accel-Redirect") != "/_internal/icons/"+currentIcons.fallbackHash {
		t.Fatal("initialization did not restore fallback")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 || files[0].Name() != currentIcons.fallbackHash {
		t.Fatalf("initialization retained old generations: files=%v err=%v", files, err)
	}
	t.Log("concurrent uploads, durable reload, empty image, failed write and initialization passed")
}
