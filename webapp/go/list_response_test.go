package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestCompletedListReusesBodiesAndRefreshesDependencies(t *testing.T) {
	s, stream, user := responseJSONFixture(t)
	s.indexStream(stream)
	ctx := context.Background()
	key := listResponseKey{limit: -1}
	one, err := s.completedList(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.completedList(ctx, key)
	if err != nil || one != two {
		t.Fatal("completed response was not reused", err)
	}
	want, err := fillLivestreamResponses(ctx, nil, s.searchStreams(-1, 0, false))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(want)
	if !bytes.Equal(one.plain, append(encoded, '\n')) {
		t.Fatal("completed JSON differs from typed response")
	}
	before := append([]byte(nil), one.plain...)
	user.icon.hash.Store("new-hash")
	three, err := s.completedList(ctx, key)
	if err != nil || three == one || bytes.Equal(three.plain, one.plain) || !bytes.Equal(one.plain, before) {
		t.Fatal("icon update/immutable payload failed", err)
	}
	old := currentTags.Load()
	currentTags.Store(&tagCatalog{byID: map[int64]Tag{1: {ID: 1, Name: "changed"}, 2: old.byID[2]}})
	four, err := s.completedList(ctx, key)
	if err != nil || four == three || bytes.Equal(four.plain, three.plain) {
		t.Fatal("tag update reused a stale response", err)
	}
	newStream := &livestreamReference{model: LivestreamModel{ID: 3, UserID: 1}, tagIDs: []int64{2, 2}}
	s.livestreams.Store(int64(3), newStream)
	s.indexStream(newStream)
	for _, tc := range []struct {
		key listResponseKey
		ids []int64
	}{
		{key, []int64{3, 2}}, {listResponseKey{limit: 1}, []int64{3}},
		{listResponseKey{limit: 0}, []int64{}}, {listResponseKey{kind: 2, id: 1}, []int64{2, 3}},
		{listResponseKey{kind: 1, id: 2}, []int64{3, 3, 2, 2}},
	} {
		result, err := s.completedList(ctx, tc.key)
		if err != nil {
			t.Fatal(err)
		}
		var rows []Livestream
		if err := json.Unmarshal(result.plain, &rows); err != nil {
			t.Fatal(err)
		}
		ids := make([]int64, len(rows))
		for i, row := range rows {
			ids[i] = row.ID
		}
		if !reflect.DeepEqual(ids, tc.ids) {
			t.Fatalf("ids=%v want=%v", ids, tc.ids)
		}
	}
}

func TestCompletedListEncodingAndConcurrentRefresh(t *testing.T) {
	s, stream, user := responseJSONFixture(t)
	// A duplicated tag link increases size without changing response semantics.
	stream.tagIDs = append(stream.tagIDs, 2, 2, 2, 2, 2, 2, 2, 2)
	s.indexStream(stream)
	ctx := context.Background()
	for _, pretty := range []bool{false, true} {
		key := listResponseKey{limit: -1, pretty: pretty}
		result, err := s.completedList(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.gzip) == 0 {
			t.Fatal("fixture should be compressed")
		}
		for _, encoding := range []string{"gzip", "identity", "gzip;q=0, *;q=1"} {
			rec := httptest.NewRecorder()
			path := "/"
			if pretty {
				path += "?pretty"
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Accept-Encoding", encoding)
			if err := s.writeCompletedList(echo.New().NewContext(req, rec), listResponseKey{limit: -1}); err != nil {
				t.Fatal(err)
			}
			body := rec.Body.Bytes()
			if encoding == "gzip" {
				if rec.Header().Get("Content-Encoding") != "gzip" || !bytes.Equal(body, result.gzip) {
					t.Fatal("cached gzip was not served")
				}
				reader, err := gzip.NewReader(bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				body, err = io.ReadAll(reader)
				reader.Close()
				if err != nil {
					t.Fatal(err)
				}
			} else if rec.Header().Get("Content-Encoding") != "" {
				t.Fatal("gzip exclusion ignored")
			}
			if !bytes.Equal(body, result.plain) || rec.Header().Get("Vary") != "Accept-Encoding" {
				t.Fatal("encoding changed completed JSON")
			}
		}
	}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := 0; i < 100; i++ {
			user.icon.hash.Store("A")
			user.icon.hash.Store("B")
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 100; i++ {
			response, err := s.completedList(ctx, listResponseKey{limit: -1})
			if err != nil || !json.Valid(response.plain) {
				t.Error("concurrent response publication failed", err)
				return
			}
		}
	}()
	workers.Wait()
}
