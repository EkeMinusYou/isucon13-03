package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
)

func responseJSONFixture(t *testing.T) (*referenceStore, *livestreamReference, *userReference) {
	t.Helper()
	text := "日本語Ω😀\x00\b\f\n\r\t\"\\<>&\u2028\u2029" + string([]byte{0xff})
	store := &referenceStore{}
	icon := &iconEntry{}
	icon.hash.Store("first-hash")
	user := &userReference{user: User{ID: 1, Name: text, DisplayName: text, Description: text, Theme: Theme{ID: 2, DarkMode: true}}, icon: icon}
	store.users.Store(int64(1), user)
	stream := &livestreamReference{model: LivestreamModel{ID: 2, UserID: 1, Title: text, Description: text, PlaylistUrl: text, ThumbnailUrl: text, StartAt: -1, EndAt: 1 << 62}, tagIDs: []int64{1, 2, 2}}
	store.livestreams.Store(int64(2), stream)
	previousRefs := currentReferences.Swap(store)
	previousTags := currentTags.Swap(&tagCatalog{byID: map[int64]Tag{1: {ID: 1, Name: text}, 2: {ID: 2, Name: "second"}}})
	t.Cleanup(func() { currentReferences.Store(previousRefs); currentTags.Store(previousTags) })
	return store, stream, user
}

func TestResponseJSONMatchesTypedResponses(t *testing.T) {
	_, stream, user := responseJSONFixture(t)
	ctx := context.Background()
	comments := []LivecommentModel{
		{ID: 1 << 62, UserID: 1, LivestreamID: 2, Comment: user.user.Name, Tip: -1 << 62, CreatedAt: 3},
		{ID: 4, UserID: 1, LivestreamID: 2, Comment: "", Tip: 0, CreatedAt: 3},
	}
	reactions := []ReactionModel{{ID: 7, UserID: 1, LivestreamID: 2, EmojiName: user.user.Name, CreatedAt: -1}}
	streams := []*LivestreamModel{&stream.model, &stream.model}
	check := func(name string, data *responseJSONBuffer, err error, want any) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		defer releaseResponseJSON(data)
		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data.data, encoded) {
			t.Fatalf("%s differs from the typed response: got=%s want=%s", name, data.data, encoded)
		}
		for _, pretty := range []bool{false, true} {
			rec := httptest.NewRecorder()
			path := "/"
			if pretty {
				path += "?pretty"
			}
			c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, path, nil), rec)
			// writeResponseJSON appends a newline to compact responses.
			data.data = data.data[:len(encoded)]
			if err := writeResponseJSON(c, data); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != echo.MIMEApplicationJSONCharsetUTF8 {
				t.Fatalf("incorrect response headers: %d %v", rec.Code, rec.Header())
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, rec.Body.Bytes()); err != nil || !bytes.Equal(compact.Bytes(), encoded) {
				t.Fatalf("%s pretty=%t changed JSON: %s %v", name, pretty, rec.Body.String(), err)
			}
		}
	}
	for _, empty := range []bool{false, true} {
		if empty {
			comments = nil
			reactions = nil
			streams = nil
		}
		wantComments, err := fillLivecommentResponses(ctx, nil, comments)
		if err != nil {
			t.Fatal(err)
		}
		data, err := livecommentResponseJSON(ctx, comments)
		check("comments", data, err, wantComments)
		wantReactions, err := fillReactionResponses(ctx, nil, reactions)
		if err != nil {
			t.Fatal(err)
		}
		data, err = reactionResponseJSON(ctx, reactions)
		check("reactions", data, err, wantReactions)
		wantStreams, err := fillLivestreamResponses(ctx, nil, streams)
		if err != nil {
			t.Fatal(err)
		}
		data, err = livestreamResponseJSON(ctx, streams)
		check("streams", data, err, wantStreams)
	}
}

func TestResponseJSONUpdatesAndPublication(t *testing.T) {
	store, stream, user := responseJSONFixture(t)
	ctx := context.Background()
	models := []LivecommentModel{{ID: 1, UserID: 1, LivestreamID: 2, Comment: "retained"}}
	old, err := livecommentResponseJSON(ctx, models)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseResponseJSON(old)
	before := append([]byte(nil), old.data...)
	user.icon.hash.Store("updated-hash")
	currentTags.Store(&tagCatalog{byID: map[int64]Tag{1: {ID: 1, Name: "changed"}, 2: {ID: 2, Name: "second"}}})
	updated, err := livecommentResponseJSON(ctx, models)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseResponseJSON(updated)
	var rows []Livecomment
	if err := json.Unmarshal(updated.data, &rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].User.IconHash != "updated-hash" || rows[0].Livestream.Owner.IconHash != "updated-hash" || rows[0].Livestream.Tags[0].Name != "changed" || len(rows[0].Livestream.Tags) != 3 {
		t.Fatalf("cached dependencies were not refreshed: %+v", rows)
	}
	if !bytes.Equal(old.data, before) {
		t.Fatal("a published response buffer was mutated")
	}
	// A request keeps a consistent user snapshot across its rows and nested owner.
	builder := newResponseJSONBuilder(ctx)
	snapshot, err := builder.user(1)
	if err != nil {
		t.Fatal(err)
	}
	user.icon.hash.Store("next-hash")
	data, err := builder.stream(2)
	if err != nil {
		t.Fatal(err)
	}
	var gotStream Livestream
	if err := json.Unmarshal(data, &gotStream); err != nil {
		t.Fatal(err)
	}
	if gotStream.Owner.IconHash != snapshot.hash {
		t.Fatal("one response mixed icon versions")
	}
	// Even a warm cache must wait for reference commit/publication.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	user.ready = make(chan struct{})
	if _, err := store.userJSON(cancelled, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("unpublished user: %v", err)
	}
	close(user.ready)
	stream.ready = make(chan struct{})
	if _, err := newResponseJSONBuilder(cancelled).stream(2); !errors.Is(err, context.Canceled) {
		t.Fatalf("unpublished stream: %v", err)
	}
	close(stream.ready)
}

func TestResponseJSONConcurrentIconChanges(t *testing.T) {
	_, _, user := responseJSONFixture(t)
	models := []LivecommentModel{{ID: 1, UserID: 1, LivestreamID: 2, Comment: "concurrent"}}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				user.icon.hash.Store("first-hash")
				if i%2 == 0 {
					user.icon.hash.Store("second-hash")
				}
				data, err := livecommentResponseJSON(context.Background(), models)
				if err != nil {
					t.Error(err)
					return
				}
				var rows []Livecomment
				err = json.Unmarshal(data.data, &rows)
				releaseResponseJSON(data)
				if err != nil || len(rows) != 1 || rows[0].User.IconHash != rows[0].Livestream.Owner.IconHash {
					t.Errorf("concurrent cache publication: %+v %v", rows, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
