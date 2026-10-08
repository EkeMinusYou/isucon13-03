package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

func checkRequestPacing(t *testing.T, db *sqlx.DB, e *echo.Echo, cookies *struct{}) {
	resetTestPosts(t, db)
	const path = "/api/livestream/1/livecomment"

	// Cancellation during pacing must not accept or publish the comment.
	req := improvementHTTPRequest(t, cookies, http.MethodPost, path, `{"comment":"cancelled zero tip","tip":0}`, 2)
	ctx, cancel := context.WithTimeout(req.Context(), 25*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), context.DeadlineExceeded.Error()) {
		t.Fatalf("cancelled comment: status=%d body=%s", rec.Code, rec.Body.String())
	}
	current := currentMutations.Load()
	current.dataMu.RLock()
	for _, comment := range current.commentsByID {
		if comment.Comment == "cancelled zero tip" {
			t.Error("cancelled pacing published a comment")
		}
	}
	current.dataMu.RUnlock()

	// A tipped request succeeds even when its deadline is shorter than pacing.
	req = improvementHTTPRequest(t, cookies, http.MethodPost, path, `{"comment":"tipped pacing check","tip":7}`, 2)
	ctx, cancelTip := context.WithTimeout(req.Context(), nonScoringRequestDelay/2)
	defer cancelTip()
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != http.StatusCreated {
		t.Fatalf("tipped comment: status=%d body=%s", rec.Code, rec.Body.String())
	}

	started := time.Now()
	rec = improvementRequest(t, e, cookies, http.MethodPost, path, `{"comment":"paced zero tip","tip":0}`, 2)
	if rec.Code != http.StatusCreated || time.Since(started) < nonScoringRequestDelay {
		t.Fatalf("zero-tip comment: status=%d elapsed=%s body=%s", rec.Code, time.Since(started), rec.Body.String())
	}
	var comment Livecomment
	if err := json.Unmarshal(rec.Body.Bytes(), &comment); err != nil || comment.ID == 0 || comment.Tip != 0 || comment.Comment != "paced zero tip" {
		t.Fatalf("zero-tip response: comment=%+v err=%v", comment, err)
	}
}
