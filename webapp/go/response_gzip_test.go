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
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestTimelineGzipNegotiationAndContent(t *testing.T) {
	_, _, user := responseJSONFixture(t)
	models := []LivecommentModel{{ID: 1, UserID: 1, LivestreamID: 2, Comment: strings.Repeat(user.user.Name, 64), Tip: 37}}
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
				data, err := livecommentResponseJSON(context.Background(), models)
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
				if err := writeCompressedResponseJSON(c, data); err != nil {
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

func TestTimelineGzipScopeAndConcurrentReuse(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, compressed := range []bool{false, true} {
				for _, text := range []string{"[]", `[{"comment":"` + strings.Repeat("concurrent JSON", 1000) + `"}]`} {
					b := &responseJSONBuffer{data: []byte(text)}
					req := httptest.NewRequest(http.MethodGet, "/", nil)
					req.Header.Set("Accept-Encoding", "gzip")
					rec := httptest.NewRecorder()
					c := echo.New().NewContext(req, rec)
					if err := writeResponseJSONEncoding(c, b, compressed); err != nil {
						t.Error(err)
						return
					}
					encoded := rec.Header().Get("Content-Encoding") == "gzip"
					if encoded != (compressed && len(text) >= 1024) {
						t.Errorf("unexpected compression scope: %t %d", compressed, len(text))
						return
					}
					body := rec.Body.Bytes()
					if encoded {
						z, err := gzip.NewReader(bytes.NewReader(body))
						if err != nil {
							t.Error(err)
							return
						}
						body, err = io.ReadAll(z)
						z.Close()
						if err != nil {
							t.Error(err)
							return
						}
					}
					if string(body) != text+"\n" {
						t.Error("pooled compressor mixed responses")
					}
				}
			}
		}()
	}
	wg.Wait()
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
