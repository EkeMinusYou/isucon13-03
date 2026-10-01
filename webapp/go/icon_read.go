package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

const iconLease = 750 * time.Millisecond
const iconSyncInterval = 100 * time.Millisecond

type iconChange struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}
type iconSnapshot struct {
	Epoch    string       `json:"epoch"`
	Revision uint64       `json:"revision"`
	Full     bool         `json:"full"`
	Changes  []iconChange `json:"changes"`
}

type iconUpdate struct {
	Epoch    string `json:"epoch"`
	Revision uint64 `json:"revision"`
	Name     string `json:"name"`
	Hash     string `json:"hash"`
}

// Called under the store lock, including publication of the current hash.
func (s *iconStore) changedIcon(name, hash string) {
	s.revision++
	s.changes = append(s.changes, iconChange{Name: name, Hash: hash})
	if len(s.changes) > 8192 {
		s.changes = append([]iconChange(nil), s.changes[4096:]...)
		s.changeBase += 4096
	}
}

func privateIconClient(r *http.Request, allowed string) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	return err == nil && (host == "127.0.0.1" || host == "::1" || allowed != "" && host == allowed)
}

func iconSnapshotHandler(c echo.Context) error {
	if !privateIconClient(c.Request(), os.Getenv("ISUPIPE_ICON_READ_IP")) {
		return echo.NewHTTPError(http.StatusForbidden)
	}
	s := currentIcons
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	s.mu.RLock()
	after, _ := strconv.ParseUint(c.QueryParam("after"), 10, 64)
	full := c.QueryParam("epoch") != s.epoch || after < s.changeBase || after > s.revision
	snap := iconSnapshot{Epoch: s.epoch, Revision: s.revision, Full: full}
	if full {
		snap.Changes = make([]iconChange, 0, len(s.byName))
		for name, entry := range s.byName {
			snap.Changes = append(snap.Changes, iconChange{Name: name, Hash: entry.hash.Load().(string)})
		}
	} else {
		snap.Changes = append([]iconChange(nil), s.changes[after-s.changeBase:]...)
	}
	s.mu.RUnlock()
	return c.JSON(http.StatusOK, snap)
}

type iconReplica struct {
	mu           sync.RWMutex
	syncMu       sync.Mutex
	byName       map[string]string
	epoch        string
	revision     uint64
	validUntil   time.Time
	invalidation uint64
	changes      []iconChange
	changeBase   uint64
	edgeWake     chan struct{}
	origin       string
	client       *http.Client
	proxy        *httputil.ReverseProxy
}

func newIconReplica(origin string) (*iconReplica, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return nil, fmt.Errorf("invalid icon origin")
	}
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, MaxIdleConnsPerHost: 32, ResponseHeaderTimeout: time.Second}
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.Transport = transport
	return &iconReplica{origin: strings.TrimRight(origin, "/"), client: &http.Client{Transport: transport, Timeout: time.Second}, proxy: proxy, byName: make(map[string]string), edgeWake: make(chan struct{}, 1)}, nil
}

func (s *iconReplica) refresh(ctx context.Context) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	s.mu.RLock()
	epoch, revision, invalidation := s.epoch, s.revision, s.invalidation
	s.mu.RUnlock()
	started := time.Now()
	endpoint := s.origin + "/_internal/icon-state?epoch=" + url.QueryEscape(epoch) + "&after=" + strconv.FormatUint(revision, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("icon snapshot: HTTP %d", resp.StatusCode)
	}
	var snap iconSnapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&snap); err != nil {
		return err
	}
	if snap.Epoch == "" || (!snap.Full && (snap.Epoch != epoch || snap.Revision < revision || uint64(len(snap.Changes)) != snap.Revision-revision)) {
		return fmt.Errorf("invalid icon snapshot version")
	}
	for _, change := range snap.Changes {
		if !validIconHash(change.Hash) {
			return fmt.Errorf("invalid icon snapshot hash")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.invalidation != invalidation {
		return fmt.Errorf("icon snapshot invalidated in flight")
	}
	if snap.Full {
		s.byName = make(map[string]string, len(snap.Changes))
		s.changes, s.changeBase = nil, snap.Revision
	} else {
		s.appendChanges(snap.Changes)
	}
	for _, change := range snap.Changes {
		s.byName[change.Name] = change.Hash
	}
	s.epoch, s.revision = snap.Epoch, snap.Revision
	// Start at request dispatch, so a slow response cannot prolong old metadata.
	s.validUntil = started.Add(iconLease)
	s.wakeEdge()
	return nil
}

func (s *iconReplica) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_internal/update" {
		if r.Method != http.MethodPost || !privateIconClient(r, os.Getenv("ISUPIPE_ICON_ORIGIN_IP")) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var update iconUpdate
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&update); err != nil || update.Epoch == "" || !validIconHash(update.Hash) {
			http.Error(w, "invalid update", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.invalidation++
		if update.Epoch == s.epoch && update.Revision == s.revision+1 {
			s.byName[update.Name] = update.Hash
			s.appendChanges([]iconChange{{Name: update.Name, Hash: update.Hash}})
			s.revision = update.Revision
		} else if update.Epoch != s.epoch || update.Revision > s.revision {
			// An out-of-order notification cannot certify the other names.
			s.validUntil = time.Time{}
		}
		s.wakeEdge()
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/_internal/invalidate" {
		if r.Method != http.MethodPost || !privateIconClient(r, os.Getenv("ISUPIPE_ICON_ORIGIN_IP")) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		s.mu.Lock()
		s.invalidation++
		s.validUntil = time.Time{}
		s.epoch, s.revision = "", 0
		s.changes, s.changeBase = nil, 0
		s.wakeEdge()
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/_internal/status" && (privateIconClient(r, os.Getenv("ISUPIPE_ICON_ORIGIN_IP")) || privateIconClient(r, os.Getenv("ISUPIPE_ICON_READ_IP"))) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"epoch": s.epoch, "revision": s.revision, "users": len(s.byName), "fresh": time.Now().Before(s.validUntil)})
		return
	}
	const prefix, suffix = "/api/user/", "/icon"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimRight(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), suffix), " ")
	if name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	s.mu.RLock()
	hash, known := s.byName[name]
	fresh := time.Now().Before(s.validUntil)
	s.mu.RUnlock()
	if fresh && known && iconETagMatches(r.Header.Get("If-None-Match"), hash) {
		if sess, err := sessionFromRequest(r); err == nil {
			w.Header().Set("X-Measurement-Session-ID", sess.ID)
		}
		w.Header().Set("ETag", "\""+hash+"\"")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	// Image bodies, unknown users, and expired leases use the durable owner.
	s.proxy.ServeHTTP(w, r)
}

func runIconRead() error {
	s, err := newIconReplica(os.Getenv("ISUPIPE_ICON_ORIGIN_URL"))
	if err != nil {
		return err
	}
	if endpoint := os.Getenv("ISUPIPE_ICON_EDGE_URL"); endpoint != "" {
		go s.runEdgePublisher(context.Background(), endpoint)
	}
	go func() {
		ticker := time.NewTicker(iconSyncInterval)
		defer ticker.Stop()
		for {
			_ = s.refresh(context.Background())
			<-ticker.C
		}
	}()
	if socket := os.Getenv("ISUPIPE_ICON_READ_SOCKET"); socket != "" {
		if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
			return err
		}
		listener, err := net.Listen("unix", socket)
		if err != nil {
			return err
		}
		if err := os.Chmod(socket, 0666); err != nil {
			listener.Close()
			return err
		}
		go func() {
			if err := (&http.Server{Handler: s, ReadHeaderTimeout: time.Second}).Serve(listener); err != nil {
				log.Fatal(err)
			}
		}()
	}
	return (&http.Server{Addr: os.Getenv("ISUPIPE_ICON_READ_LISTEN"), Handler: s, ReadHeaderTimeout: time.Second}).ListenAndServe()
}

func invalidateIconReplica() {
	endpoint := os.Getenv("ISUPIPE_ICON_READ_URL")
	if endpoint == "" {
		return
	}
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Post(endpoint+"/_internal/invalidate", "application/json", nil)
	if err == nil {
		resp.Body.Close()
	}
}

var iconNotifyClient = &http.Client{Timeout: 500 * time.Millisecond}

func notifyIconReplica(update iconUpdate) {
	endpoint := os.Getenv("ISUPIPE_ICON_READ_URL")
	if endpoint == "" {
		return
	}
	data, err := json.Marshal(update)
	if err != nil {
		return
	}
	resp, err := iconNotifyClient.Post(endpoint+"/_internal/update", "application/json", bytes.NewReader(data))
	if err == nil {
		resp.Body.Close()
	}
}
