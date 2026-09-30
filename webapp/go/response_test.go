package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

func checkResponseBatching(t *testing.T, db *sqlx.DB, e *echo.Echo, store *sessions.CookieStore) {
	ctx := context.Background()
	fallback, err := os.ReadFile(fallbackImage)
	if err != nil {
		t.Fatal(err)
	}
	wantUsers := map[int64]User{}
	for _, entry := range []struct {
		id    int64
		name  string
		image []byte
	}{{1, "alpha", []byte("first-icon")}, {2, "beta", []byte{}}, {3, "empty", fallback}, {4, "éclair", fallback}, {5, "Ωmega", fallback}} {
		hash := sha256.Sum256(entry.image)
		wantUsers[entry.id] = User{
			ID: entry.id, Name: entry.name, Theme: Theme{ID: entry.id, DarkMode: entry.id == 1},
			IconHash: fmt.Sprintf("%x", hash),
		}
	}
	wantStream := Livestream{ID: 1, Owner: wantUsers[1], Tags: []Tag{{ID: 1, Name: "tag-a"}, {ID: 2, Name: "tag-b"}, {ID: 2, Name: "tag-b"}}, EndAt: 1}
	read := func(path string, into interface{}) {
		t.Helper()
		rec := improvementRequest(t, e, store, http.MethodGet, path, "", 1)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range wantUsers {
		var got User
		read("/api/user/"+want.Name, &got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("user: got=%+v want=%+v", got, want)
		}
	}
	var stream Livestream
	read("/api/livestream/1", &stream)
	if !reflect.DeepEqual(stream, wantStream) {
		t.Fatalf("livestream: got=%+v want=%+v", stream, wantStream)
	}
	for _, tc := range []struct {
		path string
		ids  []int64
	}{
		{"/api/livestream/search?limit=2", []int64{6, 5}},
		{"/api/livestream/search?tag=tag-b&limit=1", []int64{2, 1, 1}},
		{"/api/livestream", []int64{1, 2}},
		{"/api/user/beta/livestream", []int64{3}},
	} {
		var streams []Livestream
		read(tc.path, &streams)
		ids := make([]int64, len(streams))
		for i, stream := range streams {
			ids[i] = stream.ID
			if stream.Tags == nil || !reflect.DeepEqual(stream.Owner, wantUsers[stream.Owner.ID]) {
				t.Fatalf("%s: incorrect nested response: %+v", tc.path, stream)
			}
		}
		if !reflect.DeepEqual(ids, tc.ids) {
			t.Fatalf("%s: order/duplicates got=%v want=%v", tc.path, ids, tc.ids)
		}
	}
	var comments []Livecomment
	read("/api/livestream/1/livecomment", &comments)
	wantComments := []Livecomment{
		{ID: 2, User: wantUsers[2], Livestream: wantStream, Comment: "second", Tip: 3, CreatedAt: 2},
		{ID: 1, User: wantUsers[1], Livestream: wantStream, Comment: "first", Tip: 2, CreatedAt: 1},
	}
	if !reflect.DeepEqual(comments, wantComments) {
		t.Fatalf("timeline: got=%+v want=%+v", comments, wantComments)
	}
	read("/api/livestream/1/livecomment?limit=1", &comments)
	if !reflect.DeepEqual(comments, wantComments[:1]) {
		t.Fatalf("limited timeline: %+v", comments)
	}
	var reports []LivecommentReport
	read("/api/livestream/1/report", &reports)
	if !reflect.DeepEqual(reports, []LivecommentReport{
		{ID: 1, Reporter: wantUsers[1], Livecomment: wantComments[1]},
		{ID: 2, Reporter: wantUsers[2], Livecomment: wantComments[0]},
	}) {
		t.Fatalf("reports: %+v", reports)
	}
	var reactions []Reaction
	read("/api/livestream/1/reaction?limit=1", &reactions)
	if !reflect.DeepEqual(reactions, []Reaction{{ID: 2, User: wantUsers[1], Livestream: wantStream, EmojiName: "b", CreatedAt: 2}}) {
		t.Fatalf("reaction: %+v", reactions)
	}
	for _, path := range []string{"/api/livestream/6/livecomment", "/api/livestream/6/reaction", "/api/livestream/search?limit=0"} {
		rec := improvementRequest(t, e, store, http.MethodGet, path, "", 1)
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("%s: empty result status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	// The number of statements must not grow with repeated timeline entries.
	queryCount := func(models []LivecommentModel) int {
		t.Helper()
		tx, err := db.BeginTxx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		questions := func() int {
			var row struct {
				Name  string `db:"Variable_name"`
				Value string `db:"Value"`
			}
			if err := tx.GetContext(ctx, &row, "SHOW SESSION STATUS LIKE 'Questions'"); err != nil {
				t.Fatal(err)
			}
			value, err := strconv.Atoi(row.Value)
			if err != nil {
				t.Fatal(err)
			}
			return value
		}
		before := questions()
		responses, err := fillLivecommentResponses(ctx, tx, models)
		if err != nil || len(responses) != len(models) {
			t.Fatalf("batch response: len=%d err=%v", len(responses), err)
		}
		return questions() - before
	}
	model := LivecommentModel{ID: 1, UserID: 1, LivestreamID: 1, Comment: "first", Tip: 2, CreatedAt: 1}
	large := make([]LivecommentModel, 100)
	for i := range large {
		large[i] = model
	}
	smallCount, largeCount := queryCount([]LivecommentModel{model}), queryCount(large)
	if smallCount != largeCount {
		t.Fatalf("statement count grows with rows: one=%d hundred=%d", smallCount, largeCount)
	}
	t.Logf("timeline SQL count is constant: one=%d hundred=%d (includes status read)", smallCount, largeCount)
	// An upload must be visible in the next nested response.
	post := func(image string) {
		t.Helper()
		body, _ := json.Marshal(PostIconRequest{Image: []byte(image)})
		rec := improvementRequest(t, e, store, http.MethodPost, "/api/icon", string(body), 1)
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload icon: status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	post("updated-icon")
	defer post("first-icon")
	read("/api/livestream/1", &stream)
	hash := sha256.Sum256([]byte("updated-icon"))
	if stream.Owner.IconHash != fmt.Sprintf("%x", hash) {
		t.Fatalf("icon update was not reflected: %+v", stream.Owner)
	}
	// Reloading after initialization replaces the entire immutable tag catalog.
	if _, err := db.Exec("UPDATE tags SET name='reloaded-tag' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := db.Exec("UPDATE tags SET name='tag-a' WHERE id=1"); err != nil {
			t.Error(err)
		}
		if err := reloadTags(ctx); err != nil {
			t.Error(err)
		}
	}()
	if err := reloadTags(ctx); err != nil {
		t.Fatal(err)
	}
	var tags TagsResponse
	read("/api/tag", &tags)
	if !reflect.DeepEqual(tags.Tags, []Tag{{ID: 1, Name: "reloaded-tag"}, {ID: 2, Name: "tag-b"}}) {
		t.Fatalf("reloaded tags: %+v", tags)
	}
}

func checkReservationAtomicity(t *testing.T, db *sqlx.DB, e *echo.Echo, store *sessions.CookieStore) {
	const start int64 = 1700874000
	if _, err := db.Exec(`INSERT INTO reservation_slots (id,slot,start_at,end_at) VALUES
 (1,1,?,?), (2,0,?,?), (3,2,?,?), (4,7,?,?)`,
		start, start+3600, start+3600, start+7200, start+7200, start+10800, start-3600, start); err != nil {
		t.Fatal(err)
	}
	body := func(end int64) string {
		data, err := json.Marshal(ReserveLivestreamRequest{StartAt: start, EndAt: end, Tags: []int64{2, 1}})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if rec := improvementRequest(t, e, store, http.MethodPost, "/api/livestream/reservation", body(start+7200), 1); rec.Code != http.StatusBadRequest {
		t.Fatalf("partly exhausted reservation: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Prepare both authenticated requests before racing for the final slot.
	requests := make([]*http.Request, 2)
	for i := range requests {
		req := httptest.NewRequest(http.MethodPost, "/api/livestream/reservation", strings.NewReader(body(start+3600)))
		req.Header.Set("Content-Type", "application/json")
		sess, err := store.New(req, defaultSessionIDKey)
		if err != nil {
			t.Fatal(err)
		}
		sess.Values[defaultUserIDKey] = int64(i + 1)
		sess.Values[defaultSessionExpiresKey] = time.Now().Add(time.Minute).Unix()
		cookieResponse := httptest.NewRecorder()
		if err := store.Save(req, cookieResponse, sess); err != nil {
			t.Fatal(err)
		}
		for _, cookie := range cookieResponse.Result().Cookies() {
			req.AddCookie(cookie)
		}
		requests[i] = req
	}
	ready := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	for _, req := range requests {
		go func(req *http.Request) {
			<-ready
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			results <- rec
		}(req)
	}
	close(ready)
	statuses := map[int]int{}
	for range requests {
		select {
		case rec := <-results:
			statuses[rec.Code]++
			if rec.Code == http.StatusCreated {
				var stream Livestream
				if err := json.Unmarshal(rec.Body.Bytes(), &stream); err != nil || stream.ID == 0 || len(stream.Tags) != 2 {
					t.Fatalf("reserved stream response: body=%s err=%v", rec.Body.String(), err)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent reservation timed out")
		}
	}
	if statuses[http.StatusCreated] != 1 || statuses[http.StatusBadRequest] != 1 {
		t.Fatalf("final slot must be reserved once: %v", statuses)
	}
	var slots []int64
	if err := db.Select(&slots, "SELECT slot FROM reservation_slots ORDER BY id"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(slots, []int64{0, 0, 2, 7}) {
		t.Fatalf("slot boundaries / rollback / overbooking: %v", slots)
	}
}
