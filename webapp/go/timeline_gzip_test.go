package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
)

func timelineCacheMembers(s *referenceStore) (members map[[32]byte][]byte, size int) {
	members = make(map[[32]byte][]byte)
	for i := range s.timelineGzip {
		cache := &s.timelineGzip[i]
		cache.mu.Lock()
		for key, member := range cache.entries {
			members[key] = member
		}
		size += cache.bytes
		cache.mu.Unlock()
	}
	return
}

func checkGroupedTimeline(t *testing.T, models []LivecommentModel, pretty bool) []byte {
	t.Helper()
	b, err := livecommentResponseJSON(context.Background(), models)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseResponseJSON(b)
	want, err := fillLivecommentResponses(context.Background(), nil, models)
	if err != nil {
		t.Fatal(err)
	}
	path := "/"
	if pretty {
		path += "?pretty"
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	if err := writeCompressedResponseJSON(echo.New().NewContext(req, rec), b); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.Bytes()
	if rec.Header().Get("Content-Encoding") == "gzip" {
		z, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		body, err = io.ReadAll(z)
		z.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	encoded, _ := json.Marshal(want)
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil || !bytes.Equal(compact.Bytes(), encoded) || body[len(body)-1] != '\n' {
		t.Fatalf("grouped JSON differs from typed response: %v", err)
	}
	legacy, err := compressResponseJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) > 0 {
		ratio := float64(rec.Body.Len()) / float64(len(legacy))
		t.Logf("rows=%d pretty=%t legacy=%d grouped=%d ratio=%.3f", len(models), pretty, len(legacy), rec.Body.Len(), ratio)
		if ratio > 1.3 {
			t.Fatalf("group compression increases transfer too much: %.3f", ratio)
		}
	}
	return append([]byte(nil), rec.Body.Bytes()...)
}

func TestTimelineGzipGroupsReuseAndInvalidation(t *testing.T) {
	refs, _, user := responseJSONFixture(t)
	models := make([]LivecommentModel, 256)
	for i := range models {
		models[i] = LivecommentModel{ID: int64(1000 - i), UserID: 1, LivestreamID: 2,
			Comment: fmt.Sprintf("comment %d 日本語 <>&", i), Tip: int64(i), CreatedAt: int64(1000 - i)}
	}
	old := checkGroupedTimeline(t, models, false)
	members, _ := timelineCacheMembers(refs)
	if len(members) < 2 {
		t.Fatal("fixture did not exercise multiple gzip members")
	}
	checkGroupedTimeline(t, models, false)
	next, _ := timelineCacheMembers(refs)
	if len(next) != len(members) {
		t.Fatal("unchanged timeline created new gzip groups")
	}
	for key, member := range members {
		if &next[key][0] != &member[0] {
			t.Fatal("unchanged timeline replaced a cached member")
		}
	}
	models = append([]LivecommentModel{{ID: 1001, UserID: 1, LivestreamID: 2, Comment: "prepended", CreatedAt: 1001}}, models...)
	checkGroupedTimeline(t, models, false)
	prepended, _ := timelineCacheMembers(refs)
	if len(prepended)-len(next) > 2 {
		t.Fatal("prepend invalidated interior groups")
	}
	// Moderation removes a row without changing the returned order.
	models = append(models[:150:150], models[151:]...)
	checkGroupedTimeline(t, models, false)
	for _, limit := range []int{0, 1, 12, 64, 65} {
		checkGroupedTimeline(t, models[:limit], false)
		checkGroupedTimeline(t, models[:limit], true)
	}
	// Small timelines must have exactly the original whole-response encoding.
	small, err := livecommentResponseJSON(context.Background(), models[:64])
	if err != nil {
		t.Fatal(err)
	}
	if len(small.groupEnds) != 0 {
		t.Fatal("small timeline was split into multiple groups")
	}
	releaseResponseJSON(small)
	user.icon.hash.Store("changed-icon")
	currentTags.Store(&tagCatalog{byID: map[int64]Tag{1: {ID: 1, Name: "changed-tag"}, 2: {ID: 2, Name: "second"}}})
	updated := checkGroupedTimeline(t, models, false)
	if bytes.Equal(old, updated) {
		t.Fatal("changed timeline returned old gzip")
	}
	_, size := timelineCacheMembers(refs)
	if size > 16*timelineGzipBudget {
		t.Fatal("gzip cache exceeded its budget")
	}
}

func TestTimelineGzipGroupsHTTPDecoderAndConcurrentReuse(t *testing.T) {
	_, _, user := responseJSONFixture(t)
	models := make([]ReactionModel, 128)
	for i := range models {
		models[i] = ReactionModel{ID: int64(1000 - i), UserID: 1, LivestreamID: 2, EmojiName: user.user.Name, CreatedAt: int64(i)}
	}
	probe, err := reactionResponseJSON(context.Background(), models)
	if err != nil {
		t.Fatal(err)
	}
	if len(probe.groupEnds) == 0 {
		t.Fatal("HTTP fixture did not exercise multiple gzip members")
	}
	releaseResponseJSON(probe)
	e := echo.New()
	e.GET("/", func(c echo.Context) error {
		b, err := reactionResponseJSON(c.Request().Context(), models)
		if err != nil {
			return err
		}
		defer releaseResponseJSON(b)
		return writeCompressedResponseJSON(c, b)
	})
	server := httptest.NewServer(e)
	defer server.Close()
	want, err := fillReactionResponses(context.Background(), nil, models)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(want)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := server.Client().Get(server.URL)
			if err != nil {
				t.Error(err)
				return
			}
			defer res.Body.Close()
			data, err := io.ReadAll(res.Body)
			if err != nil || !res.Uncompressed || string(data) != string(encoded)+"\n" {
				t.Errorf("HTTP automatic gzip decoding changed response: %v uncompressed=%t", err, res.Uncompressed)
			}
		}()
	}
	wg.Wait()
}

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
