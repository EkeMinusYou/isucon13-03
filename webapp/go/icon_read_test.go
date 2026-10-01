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

func TestIconReplicaNotificationFanout(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	updates := make(chan iconUpdate, 2)
	invalidations := make(chan struct{}, 2)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_internal/update":
			var update iconUpdate
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				http.Error(w, "invalid update", 400)
				return
			}
			updates <- update
			arrived <- struct{}{}
			<-release
		case "/_internal/invalidate":
			invalidations <- struct{}{}
		default:
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	first, second := httptest.NewServer(handler), httptest.NewServer(handler)
	defer func() {
		once.Do(func() { close(release) })
		first.Close()
		second.Close()
	}()
	t.Setenv("ISUPIPE_ICON_READ_URLS", first.URL+" "+second.URL)
	update := iconUpdate{Epoch: "shared", Revision: 3, Name: "alpha", Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	done := make(chan struct{})
	go func() { notifyIconReplica(update); close(done) }()
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(time.Second):
			t.Fatal("replica notifications did not start concurrently")
		}
	}
	select {
	case <-done:
		t.Fatal("notification acknowledged before both replicas completed")
	default:
	}
	once.Do(func() { close(release) })
	<-done
	for i := 0; i < 2; i++ {
		if got := <-updates; got != update {
			t.Fatal("replicas received different updates", got)
		}
	}
	invalidateIconReplica()
	if len(invalidations) != 2 {
		t.Fatal("initialization did not invalidate both replicas")
	}
}

func TestIconReplicaClientAllowlist(t *testing.T) {
	for _, tc := range []struct {
		remote string
		allow  bool
	}{
		{"127.0.0.1:1234", true},
		{"[::1]:1234", true},
		{"192.0.2.1:1234", true},
		{"192.0.2.2:1234", true},
		{"192.0.2.3:1234", false},
		{"192.0.2.1", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/_internal/icon-state", nil)
		r.RemoteAddr = tc.remote
		if got := privateIconClient(r, "192.0.2.1 192.0.2.2"); got != tc.allow {
			t.Fatalf("%s: allowed=%v, want %v", tc.remote, got, tc.allow)
		}
	}
}

func TestIconReplicaUpdatesExpiryAndRestart(t *testing.T) {
	s := &iconStore{journal: testIconJournal(t), byName: make(map[string]*iconEntry), byID: make(map[int64]*iconEntry), epoch: "initial"}
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
