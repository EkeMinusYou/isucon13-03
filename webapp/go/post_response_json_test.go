package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestCreatedRowJSONHandlersMatchCommittedRows(t *testing.T) {
	_, _, user := responseJSONFixture(t)
	s := writerTestStore(t)
	s.addUser(1, user.user.Name)
	s.addStream(2, 1)
	s.writers.Add(1)
	go s.writePosts()
	previous := currentMutations.Swap(s)
	t.Cleanup(func() { currentMutations.Store(previous) })
	e := echo.New()
	e.POST("/api/livestream/:livestream_id/livecomment", postLivecommentHandler)
	e.POST("/api/livestream/:livestream_id/reaction", postReactionHandler)
	for _, pretty := range []bool{false, true} {
		for _, route := range []string{"livecomment", "reaction"} {
			path := "/api/livestream/2/" + route
			if pretty {
				path += "?pretty"
			}
			input := map[string]any{"comment": user.user.Name, "tip": -7, "emoji_name": user.user.Name}
			body, _ := json.Marshal(input)
			user.icon.hash.Store(fmt.Sprintf("icon-%t-%s", pretty, route))
			rec := improvementRequest(t, e, &struct{}{}, http.MethodPost, path, string(body), 1)
			if rec.Code != http.StatusCreated || rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("Content-Type") != echo.MIMEApplicationJSONCharsetUTF8 {
				t.Fatalf("POST response: %d %v %s", rec.Code, rec.Header(), rec.Body)
			}
			var want any
			comments, reactions, err := s.timeline(context.Background(), 2, 1, route == "livecomment")
			if err != nil {
				t.Fatal(err)
			}
			if route == "livecomment" {
				rows, err := fillLivecommentResponses(context.Background(), nil, comments)
				if err != nil {
					t.Fatal(err)
				}
				want = rows[0]
			} else {
				rows, err := fillReactionResponses(context.Background(), nil, reactions)
				if err != nil {
					t.Fatal(err)
				}
				want = rows[0]
			}
			encoded, _ := json.Marshal(want)
			var compact bytes.Buffer
			if err := json.Compact(&compact, rec.Body.Bytes()); err != nil || !bytes.Equal(compact.Bytes(), encoded) || !strings.HasSuffix(rec.Body.String(), "\n") {
				t.Fatalf("POST JSON differs from committed row: %v", err)
			}
		}
	}
}
