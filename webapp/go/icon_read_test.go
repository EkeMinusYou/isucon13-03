package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

func TestIconReplicaUpdatesExpiryAndRestart(t *testing.T) {
	s := &iconStore{dir: t.TempDir(), byName: make(map[string]*iconEntry), byID: make(map[int64]*iconEntry), epoch: "initial"}
	first, err := s.persist([]byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	entry := s.addUser(1, "alpha", first)
	old := currentIcons
	currentIcons = s
	t.Cleanup(func() { currentIcons = old })
	e := echo.New()
	e.Use(measurementSession)
	e.GET("/_internal/icon-state", iconSnapshotHandler)
	e.GET("/api/user/:username/icon", getIconHandler)
	origin := httptest.NewServer(e)
	defer origin.Close()
	replica, err := newIconReplica(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := replica.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	value, sess, _ := encodeSession(1, time.Now().Add(time.Hour).Unix())
	read := func(etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/user/alpha/icon", nil)
		req.AddCookie(&http.Cookie{Name: defaultSessionIDKey, Value: value})
		req.Header.Set("If-None-Match", etag)
		rec := httptest.NewRecorder()
		replica.ServeHTTP(rec, req)
		return rec
	}
	if rec := read("\"" + first + "\""); rec.Code != 304 || rec.Body.Len() != 0 || rec.Header().Get("X-Measurement-Session-ID") != sess.ID {
		t.Fatal("fresh 304 identity or body is incorrect")
	}
	if rec := read(""); rec.Code != 200 || rec.Body.String() != "first" {
		t.Fatal("unconditional GET must return an image")
	}
	second, _ := s.persist([]byte("second"))
	s.mu.Lock()
	entry.hash.Store(second)
	s.changedIcon(entry.name, second)
	update := iconUpdate{Epoch: s.epoch, Revision: s.revision, Name: entry.name, Hash: second}
	s.mu.Unlock()
	data, _ := json.Marshal(update)
	req := httptest.NewRequest("POST", "/_internal/update", bytes.NewReader(data))
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	replica.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatal("update notification rejected")
	}
	if rec := read("\"" + first + "\""); rec.Code != 200 || rec.Body.String() != "second" {
		t.Fatal("updated image not served")
	}
	if rec := read("\"" + second + "\""); rec.Code != 304 {
		t.Fatal("updated validator not served")
	}
	if err := replica.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Force a missed update: the expired replica must consult the owner.
	s.mu.Lock()
	entry.hash.Store(first)
	s.changedIcon(entry.name, first)
	s.mu.Unlock()
	replica.mu.Lock()
	replica.validUntil = time.Now().Add(-time.Second)
	replica.mu.Unlock()
	if rec := read("\"" + second + "\""); rec.Code != 200 || rec.Body.String() != "first" {
		t.Fatal("expired metadata returned stale 304")
	}
	// A generation replacement removes names absent from the new snapshot.
	s.mu.Lock()
	s.epoch, s.revision, s.changeBase, s.changes = "reset", 0, 0, nil
	s.byName = make(map[string]*iconEntry)
	s.mu.Unlock()
	if err := replica.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := read("\"" + first + "\""); rec.Code != 404 {
		t.Fatal("old generation user survived")
	}
	s.addUser(1, "alpha", first)
	replica, _ = newIconReplica(origin.URL)
	if err := replica.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := read("\"" + first + "\""); rec.Code != 304 {
		t.Fatal("replica restart did not rebuild")
	}
}

func TestIconReplicaInvalidatesInflightSnapshot(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_ = json.NewEncoder(w).Encode(iconSnapshot{Epoch: "old", Full: true})
	}))
	defer origin.Close()
	defer once.Do(func() { close(release) })
	replica, _ := newIconReplica(origin.URL)
	done := make(chan error, 1)
	go func() { done <- replica.refresh(context.Background()) }()
	<-started
	req := httptest.NewRequest("POST", "/_internal/invalidate", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	replica.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatal("invalidation failed")
	}
	once.Do(func() { close(release) })
	if err := <-done; err == nil || !replica.validUntil.IsZero() {
		t.Fatal("invalidated response renewed its lease")
	}
}
