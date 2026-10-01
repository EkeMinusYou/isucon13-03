package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIconEdgeNotificationDoesNotRenewLease(t *testing.T) {
	first := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	second := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(iconSnapshot{Epoch: "one", Full: true, Changes: []iconChange{{Name: "alpha", Hash: first}}})
	}))
	defer origin.Close()
	replica, err := newIconReplica(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := replica.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := replica.validUntil
	data, _ := json.Marshal(iconUpdate{Epoch: "one", Revision: 1, Name: "alpha", Hash: second})
	req := httptest.NewRequest("POST", "/_internal/update", bytes.NewReader(data))
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	replica.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("notification: %d", rec.Code)
	}
	status := iconEdgeStatus{PublishID: "acknowledged", Epoch: "one", Revision: 0}
	p := replica.edgePublication(status, deadline.Add(-time.Millisecond))
	if p.Mode != "delta" || len(p.Changes) != 1 || p.Changes[0].Hash != second || p.ValidUntil != deadline.UnixMilli() || p.BasePublishID != "acknowledged" {
		t.Fatal("notification did not preserve the origin deadline and revision delta")
	}
	if p := replica.edgePublication(status, deadline); p.Mode != "invalidate" || p.ValidUntil != 0 {
		t.Fatal("expired data must close publication rather than renew its lease")
	}
	status.Dirty = true
	if p := replica.edgePublication(status, deadline.Add(-time.Millisecond)); p.Mode != "full" || len(p.Changes) != 1 || p.Changes[0].Hash != second {
		t.Fatal("partial publication must be rebuilt from the current complete map")
	}
}
