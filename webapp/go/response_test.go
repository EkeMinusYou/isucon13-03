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

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

func checkResponseBatching(t *testing.T, db *sqlx.DB, e *echo.Echo, store *struct{}) {
	ctx := context.Background()
	t.Run("commit_publication", func(t *testing.T) {
		checkReferenceCommits(t, db)
	})
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
		{"/api/livestream/search?tag=tag-b%20%20&limit=invalid", []int64{2, 1, 1}},
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
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/api/livestream/search?limit=-1", http.StatusInternalServerError},
		{"/api/livestream/search?limit=invalid", http.StatusBadRequest},
		{"/api/livestream/search?tag=unknown", http.StatusInternalServerError},
	} {
		if rec := improvementRequest(t, e, store, http.MethodGet, tc.path, "", 1); rec.Code != tc.status {
			t.Fatalf("%s: status=%d want=%d", tc.path, rec.Code, tc.status)
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
	if smallCount != 1 || largeCount != 1 {
		t.Fatalf("reference reads must execute no SQL: one=%d hundred=%d (includes status read)", smallCount, largeCount)
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
	// Warm serialized references must refresh both nested icons and the catalog.
	read("/api/livestream/1/livecomment", &comments)
	read("/api/livestream/1/reaction?limit=1", &reactions)
	var searched []Livestream
	read("/api/livestream/search?tag=tag-b", &searched)
	wantHash := fmt.Sprintf("%x", hash)
	for _, nested := range []Livestream{comments[0].Livestream, reactions[0].Livestream, searched[1]} {
		if nested.Owner.IconHash != wantHash || nested.Tags[0].Name != "reloaded-tag" || len(nested.Tags) != 3 {
			t.Fatalf("serialized reference dependencies changed incorrectly: %+v", nested)
		}
	}
	if comments[1].User.IconHash != wantHash || reactions[0].User.IconHash != wantHash {
		t.Fatal("serialized author icon was not refreshed")
	}
}

func checkReferenceCommits(t *testing.T, db *sqlx.DB) {
	ctx := context.Background()
	refs := currentReferences.Load()
	insert := func(id int64) *sqlx.Tx {
		t.Helper()
		tx, err := db.BeginTxx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO users (id,name,display_name,description,password) VALUES (?,?,?,?,'')", id, fmt.Sprintf("cached-%d", id), "Cached user", "Committed metadata"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO themes (id,user_id,dark_mode) VALUES (?,?,TRUE)", id, id); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	const committedID, rolledBackID int64 = 100, 101
	user := User{ID: committedID, Name: "cached-100", DisplayName: "Cached user", Description: "Committed metadata", Theme: Theme{ID: committedID, DarkMode: true}, IconHash: currentIcons.fallbackHash}
	tx := insert(committedID)
	defer tx.Rollback()
	if _, err := refs.user(ctx, committedID); err == nil {
		t.Fatal("an uncommitted user must not be visible")
	}
	if err := refs.commitUser(tx, user); err != nil {
		t.Fatal(err)
	}
	if got, err := refs.user(ctx, committedID); err != nil || !reflect.DeepEqual(got, user) {
		t.Fatalf("committed user: got=%+v err=%v", got, err)
	}
	tx = insert(rolledBackID)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	failed := user
	failed.ID, failed.Name = rolledBackID, "cached-101"
	if err := refs.commitUser(tx, failed); err == nil {
		t.Fatal("a rolled-back transaction must not publish a user")
	}
	if _, err := refs.user(ctx, rolledBackID); err == nil {
		t.Fatal("failed commit left a cached user")
	}
	if err := reloadReferences(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := currentReferences.Load().user(ctx, committedID); err != nil || !reflect.DeepEqual(got, user) {
		t.Fatalf("user restored from MySQL: got=%+v err=%v", got, err)
	}
	if _, err := db.Exec("DELETE FROM themes WHERE user_id=?; DELETE FROM users WHERE id=?", committedID, committedID); err != nil {
		t.Fatal(err)
	}
	if err := reloadReferences(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := currentReferences.Load().user(ctx, committedID); err == nil {
		t.Fatal("initialization must replace the previous user catalog")
	}
}

func checkReservationAtomicity(t *testing.T, db *sqlx.DB, e *echo.Echo, store *struct{}) {
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
		value, _, err := encodeSession(int64(i+1), time.Now().Add(time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: defaultSessionIDKey, Value: value})

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
	var reserved Livestream
	for range requests {
		select {
		case rec := <-results:
			statuses[rec.Code]++
			if rec.Code == http.StatusCreated {
				var stream Livestream
				if err := json.Unmarshal(rec.Body.Bytes(), &stream); err != nil || stream.ID == 0 || len(stream.Tags) != 2 {
					t.Fatalf("reserved stream response: body=%s err=%v", rec.Body.String(), err)
				}
				reserved = stream
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
	for _, reload := range []bool{false, true} {
		if reload {
			if err := reloadReferences(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		rec := improvementRequest(t, e, store, http.MethodGet, fmt.Sprintf("/api/livestream/%d", reserved.ID), "", 1)
		var restored Livestream
		if err := json.Unmarshal(rec.Body.Bytes(), &restored); err != nil || rec.Code != http.StatusOK || !reflect.DeepEqual(restored, reserved) {
			t.Fatalf("reserved metadata reload=%v status=%d body=%s err=%v", reload, rec.Code, rec.Body.String(), err)
		}
		rec = improvementRequest(t, e, store, http.MethodGet, "/api/livestream/search?limit=1", "", 1)
		var found []Livestream
		if err := json.Unmarshal(rec.Body.Bytes(), &found); err != nil || rec.Code != http.StatusOK || len(found) != 1 || !reflect.DeepEqual(found[0], reserved) {
			t.Fatalf("reserved search reload=%v status=%d body=%s err=%v", reload, rec.Code, rec.Body.String(), err)
		}
		rec = improvementRequest(t, e, store, http.MethodGet, "/api/livestream", "", reserved.Owner.ID)
		if err := json.Unmarshal(rec.Body.Bytes(), &found); err != nil || rec.Code != http.StatusOK || len(found) == 0 || !reflect.DeepEqual(found[len(found)-1], reserved) {
			t.Fatalf("reserved user index reload=%v status=%d body=%s err=%v", reload, rec.Code, rec.Body.String(), err)
		}
	}
}
