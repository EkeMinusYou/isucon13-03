package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestSearchGzipNegotiationAndContent(t *testing.T) {
	refs, stream, _ := responseJSONFixture(t)
	stream.tagIDs = append(stream.tagIDs, 2, 2, 2, 2, 2, 2, 2, 2)
	refs.indexStream(stream)
	for _, tc := range []struct {
		accept []string
		gzip   bool
	}{
		{nil, false}, {[]string{"identity"}, false}, {[]string{"br"}, false},
		{[]string{"gzip"}, true}, {[]string{"br, GZip; q=0.5"}, true},
		{[]string{"*"}, true}, {[]string{"gzip;q=0, *;q=1"}, false},
		{[]string{"gzip;q=NaN"}, false}, {[]string{"gzip;q=invalid"}, false},
		{[]string{"gzip;q=2"}, false}, {[]string{"gzip;q=0.1;q=0.9"}, false},
		{[]string{"gzip;q=1", "gzip;q=0"}, false},
	} {
		for _, pretty := range []bool{false, true} {
			t.Run(strings.Join(tc.accept, "/")+string(rune('0'+boolInt(pretty))), func(t *testing.T) {
				data, err := livestreamResponseJSON(context.Background(), refs.searchStreams(-1, 0, false))
				if err != nil {
					t.Fatal(err)
				}
				defer releaseResponseJSON(data)
				want := append([]byte(nil), data.data...)
				path := "/"
				if pretty {
					path += "?pretty"
				}
				req := httptest.NewRequest(http.MethodGet, path, nil)
				for _, header := range tc.accept {
					req.Header.Add("Accept-Encoding", header)
				}
				rec := httptest.NewRecorder()
				rec.Header().Set("Vary", "Origin")
				c := echo.New().NewContext(req, rec)
				if err := refs.writeCompletedList(c, listResponseKey{limit: -1}); err != nil {
					t.Fatal(err)
				}
				if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != echo.MIMEApplicationJSONCharsetUTF8 || (rec.Header().Get("Content-Encoding") == "gzip") != tc.gzip {
					t.Fatalf("incorrect response: %d %v", rec.Code, rec.Header())
				}
				vary := strings.Join(rec.Header().Values("Vary"), ",")
				if !strings.Contains(vary, "Origin") || !strings.Contains(vary, "Accept-Encoding") {
					t.Fatalf("Vary: %s", vary)
				}
				body := rec.Body.Bytes()
				if tc.gzip {
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
				var compact bytes.Buffer
				if err := json.Compact(&compact, body); err != nil || !bytes.Equal(compact.Bytes(), want) || len(body) == 0 || body[len(body)-1] != '\n' {
					t.Fatalf("JSON changed after content encoding: %v", err)
				}
			})
		}
	}
}

func TestTimelineIdentityResponses(t *testing.T) {
	_, _, user := responseJSONFixture(t)
	comments := []LivecommentModel{
		{ID: 2, UserID: 1, LivestreamID: 2, Comment: strings.Repeat(user.user.Name, 64), Tip: 37},
		{ID: 1, UserID: 1, LivestreamID: 2, Comment: "older", Tip: 1},
	}
	reactions := []ReactionModel{
		{ID: 2, UserID: 1, LivestreamID: 2, EmojiName: "new"},
		{ID: 1, UserID: 1, LivestreamID: 2, EmojiName: "older"},
	}
	previous := currentMutations.Swap(&postStore{streams: map[int64]*postStream{2: {comments: comments, reactions: reactions}}})
	t.Cleanup(func() { currentMutations.Store(previous) })
	e := echo.New()
	e.GET("/api/livestream/:livestream_id/livecomment", getLivecommentsHandler)
	e.GET("/api/livestream/:livestream_id/reaction", getReactionsHandler)
	for _, route := range []string{"livecomment", "reaction"} {
		for _, query := range []string{"", "?limit=1", "?limit=0", "?limit=1&pretty"} {
			var expected any
			n := 2
			if strings.Contains(query, "limit=1") {
				n = 1
			}
			if strings.Contains(query, "limit=0") {
				n = 0
			}
			var err error
			if route == "livecomment" {
				expected, err = fillLivecommentResponses(context.Background(), nil, comments[:n])
			} else {
				expected, err = fillReactionResponses(context.Background(), nil, reactions[:n])
			}
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(expected)
			if err != nil {
				t.Fatal(err)
			}
			for _, accept := range []string{"", "gzip", "identity", "*", "gzip;q=0"} {
				path := "/api/livestream/2/" + route + query
				req := improvementHTTPRequest(t, nil, http.MethodGet, path, "", 1)
				req.Header.Set("Accept-Encoding", accept)
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("Content-Type") != echo.MIMEApplicationJSONCharsetUTF8 {
					t.Fatalf("%s accept=%q: incorrect identity response: %d %v", path, accept, rec.Code, rec.Header())
				}
				var compact bytes.Buffer
				if err := json.Compact(&compact, rec.Body.Bytes()); err != nil || !bytes.Equal(compact.Bytes(), want) || !bytes.HasSuffix(rec.Body.Bytes(), []byte("\n")) {
					t.Fatalf("%s accept=%q: JSON changed: %v", path, accept, err)
				}
			}
		}
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
