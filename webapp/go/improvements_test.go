package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

// This test creates and removes its own database. It never changes isupipe.
func TestImprovementsMySQL(t *testing.T) {
	dsn := os.Getenv("ISUCON_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set ISUCON_TEST_MYSQL_DSN through task check-app-improvements")
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.DBName = ""
	config.InterpolateParams = true
	config.MultiStatements = true
	admin, err := sqlx.Connect("mysql", config.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	database := "isupipe_improvements_test_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := admin.Exec("CREATE DATABASE `" + database + "`"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec("DROP DATABASE `" + database + "`"); err != nil {
			t.Errorf("remove test database: %v", err)
		}
	}()
	config.DBName = database
	db, err := sqlx.Connect("mysql", config.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	previous := dbConn
	dbConn = db
	defer func() { dbConn = previous }()
	previousWriter := currentMutations.Swap(nil)
	defer func() {
		if store := currentMutations.Swap(previousWriter); store != nil {
			_ = store.close()
		}
	}()

	schema, err := os.ReadFile(os.Getenv("ISUCON_TEST_SCHEMA"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(strings.Replace(string(schema), "USE `isupipe`;", "", 1)); err != nil {
		t.Fatal(err)
	}
	var collation string
	if err := db.Get(&collation, "SELECT @@collation_connection"); err != nil {
		t.Fatal(err)
	}
	if collation != "utf8mb4_general_ci" {
		t.Fatalf("unexpected driver connection collation: %s", collation)
	}
	fixture := `
INSERT INTO users (id, name, display_name, password, description) VALUES
 (1, 'alpha', '', '', ''), (2, 'beta', '', '', ''), (3, 'empty', '', '', ''),
 (4, 'éclair', '', '', ''), (5, 'Ωmega', '', '', '');
INSERT INTO livestreams (id, user_id, title, description, playlist_url, thumbnail_url, start_at, end_at) VALUES
 (1, 1, '', '', '', '', 0, 1), (2, 1, '', '', '', '', 0, 1),
 (3, 2, '', '', '', '', 0, 1), (4, 3, '', '', '', '', 0, 1),
 (5, 4, '', '', '', '', 0, 1), (6, 5, '', '', '', '', 0, 1);
INSERT INTO reactions (user_id, livestream_id, emoji_name, created_at) VALUES
 (1, 1, 'a', 0), (1, 1, 'b', 0), (2, 2, 'c', 0), (1, 3, 'a', 0);
INSERT INTO livecomments (id, user_id, livestream_id, comment, tip, created_at) VALUES
 (1, 1, 1, 'first', 2, 0), (2, 2, 1, 'second', 3, 0),
 (3, 1, 2, 'third', 7, 0), (4, 1, 3, 'fourth', 14, 0);
INSERT INTO livestream_viewers_history (user_id, livestream_id, created_at) VALUES
 (1, 1, 0), (2, 1, 0), (3, 2, 0);
INSERT INTO livecomment_reports (user_id,livestream_id,livecomment_id,created_at,comment_user_id,comment_livestream_id,comment,comment_tip,comment_created_at)
 SELECT user_id,livestream_id,id,0,user_id,livestream_id,comment,tip,created_at FROM livecomments WHERE id IN (1,2);`
	if _, err := db.Exec(fixture); err != nil {
		t.Fatal(err)
	}
	previousTags := currentTags.Load()
	defer currentTags.Store(previousTags)
	if _, err := db.Exec(`
INSERT INTO themes (id, user_id, dark_mode) VALUES
 (1,1,TRUE), (2,2,FALSE), (3,3,FALSE), (4,4,FALSE), (5,5,FALSE);
INSERT INTO tags (id,name) VALUES (1,'tag-a'), (2,'tag-b');
INSERT INTO livestream_tags (livestream_id,tag_id) VALUES (1,2), (1,1), (2,2), (1,2);
UPDATE livecomments SET created_at=id;
UPDATE reactions SET created_at=id;
UPDATE livecomment_reports SET comment_created_at=livecomment_id;`); err != nil {
		t.Fatal(err)
	}
	if err := reloadTags(context.Background()); err != nil {
		t.Fatal(err)
	}
	previousIcons := currentIcons
	defer func() { currentIcons = previousIcons }()
	resetTestPosts(t, db)
	currentIcons, err = newIconStore(context.Background(), currentMutations.Load())
	if err != nil {
		t.Fatal(err)
	}
	for id, image := range map[int64][]byte{1: []byte("first-icon"), 2: {}} {
		hash, err := currentIcons.persist(image)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("UPDATE users SET icon_hash=? WHERE id=?", hash, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := currentIcons.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	previousReferences := currentReferences.Load()
	defer currentReferences.Store(previousReferences)
	if err := reloadReferences(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	store := &struct{}{}
	e.GET("/api/user/:username/statistics", getUserStatisticsHandler)
	e.GET("/api/livestream/:livestream_id/statistics", getLivestreamStatisticsHandler)
	e.POST("/api/livestream/:livestream_id/moderate", moderateHandler)
	e.POST("/api/livestream/:livestream_id/livecomment", postLivecommentHandler)
	e.GET("/api/tag", getTagHandler)
	e.GET("/api/user/:username", getUserHandler)
	e.GET("/api/livestream/search", searchLivestreamsHandler)
	e.GET("/api/livestream", getMyLivestreamsHandler)
	e.GET("/api/user/:username/livestream", getUserLivestreamsHandler)
	e.GET("/api/livestream/:livestream_id", getLivestreamHandler)
	e.GET("/api/livestream/:livestream_id/livecomment", getLivecommentsHandler)
	e.GET("/api/livestream/:livestream_id/reaction", getReactionsHandler)
	e.GET("/api/livestream/:livestream_id/report", getLivecommentReportsHandler)
	e.POST("/api/livestream/:livestream_id/reaction", postReactionHandler)
	e.POST("/api/livestream/:livestream_id/livecomment/:livecomment_id/report", reportLivecommentHandler)
	e.POST("/api/livestream/:livestream_id/enter", enterLivestreamHandler)
	e.DELETE("/api/livestream/:livestream_id/exit", exitLivestreamHandler)
	e.POST("/api/livestream/reservation", reserveLivestreamHandler)
	e.GET("/api/user/:username/icon", getIconHandler)
	e.POST("/api/icon", postIconHandler)
	t.Run("response_batching", func(t *testing.T) { checkResponseBatching(t, db, e, store) })
	t.Run("ng_matcher", func(t *testing.T) { checkNGMatcherMySQL(t, db) })
	t.Run("icons", func(t *testing.T) { checkIconsMySQL(t, db, e, store) })

	checkUser := func(t *testing.T, name string, want UserStatistics) {
		t.Helper()
		rec := improvementRequest(t, e, store, http.MethodGet, "/api/user/"+name+"/statistics", "", 1)
		var got UserStatistics
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || got != want {
			t.Fatalf("user %s: status=%d got=%+v want=%+v body=%s", name, rec.Code, got, want, rec.Body.String())
		}
	}
	checkLivestream := func(t *testing.T, id int64, want LivestreamStatistics) {
		t.Helper()
		rec := improvementRequest(t, e, store, http.MethodGet, fmt.Sprintf("/api/livestream/%d/statistics", id), "", 1)
		var got LivestreamStatistics
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || got != want {
			t.Fatalf("livestream %d: status=%d got=%+v want=%+v body=%s", id, rec.Code, got, want, rec.Body.String())
		}
	}
	t.Run("statistics_ties_empty_and_multiple_children", func(t *testing.T) {
		checkUser(t, "alpha", UserStatistics{Rank: 2, ViewersCount: 3, TotalReactions: 3, TotalLivecomments: 3, TotalTip: 12, FavoriteEmoji: "c"})
		checkUser(t, "beta", UserStatistics{Rank: 1, TotalReactions: 1, TotalLivecomments: 1, TotalTip: 14, FavoriteEmoji: "a"})
		checkUser(t, "empty", UserStatistics{Rank: 5})
		checkUser(t, "éclair", UserStatistics{Rank: 4})
		checkUser(t, "Ωmega", UserStatistics{Rank: 3})
		checkLivestream(t, 1, LivestreamStatistics{Rank: 3, ViewersCount: 2, TotalReactions: 2, TotalReports: 2, MaxTip: 3})
		checkLivestream(t, 2, LivestreamStatistics{Rank: 2, ViewersCount: 1, TotalReactions: 1, MaxTip: 7})
		checkLivestream(t, 3, LivestreamStatistics{Rank: 1, TotalReactions: 1, MaxTip: 14})
		checkLivestream(t, 4, LivestreamStatistics{Rank: 6})
		checkLivestream(t, 5, LivestreamStatistics{Rank: 5})
		checkLivestream(t, 6, LivestreamStatistics{Rank: 4})
		for _, path := range []string{"/api/user/missing/statistics", "/api/livestream/999/statistics"} {
			if rec := improvementRequest(t, e, store, http.MethodGet, path, "", 1); rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: expected 400, got %d", path, rec.Code)
			}
		}
	})
	t.Run("statistics_after_writes", func(t *testing.T) {
		if _, err := db.Exec("DELETE FROM livecomments WHERE id = 1; INSERT INTO reactions (user_id, livestream_id, emoji_name, created_at) VALUES (1, 1, 'c', 0)"); err != nil {
			t.Fatal(err)
		}
		resetTestPosts(t, db)
		checkUser(t, "alpha", UserStatistics{Rank: 2, ViewersCount: 3, TotalReactions: 4, TotalLivecomments: 2, TotalTip: 10, FavoriteEmoji: "c"})
		checkLivestream(t, 1, LivestreamStatistics{Rank: 3, ViewersCount: 2, TotalReactions: 3, TotalReports: 2, MaxTip: 3})
	})
	t.Run("reservation_atomicity", func(t *testing.T) { checkReservationAtomicity(t, db, e, store) })
	t.Run("request_pacing", func(t *testing.T) { checkRequestPacing(t, db, e, store) })
	for _, word := range []string{"café", "a_b", "q%r", `slash\_`, ""} {
		t.Run("moderation_"+word, func(t *testing.T) {
			if _, err := db.Exec("DELETE FROM ng_words; DELETE FROM livecomments; INSERT INTO ng_words (user_id, livestream_id, word, created_at) VALUES (1, 1, 'legacy', 0)"); err != nil {
				t.Fatal(err)
			}
			comments := []string{"LEGACY", "CAFÉ", "cafe", "safe", "aXb", "a_b", "qZZr", "slash_", "slashX", "other's quote", "😀safe"}
			want := []int64{}
			remaining := int64(0)
			for i, comment := range comments {
				id := int64(i + 1)
				if _, err := db.Exec("INSERT INTO livecomments (id, user_id, livestream_id, comment, tip, created_at) VALUES (?, 1, 1, ?, 1, 0), (?, 1, 3, ?, 1, 0)", id, comment, id+1000, comment); err != nil {
					t.Fatal(err)
				}
				matched := false
				for _, ng := range []string{"legacy", word} {
					var hit int
					// Evaluate the original parameter-only predicate as the oracle.
					if err := db.Get(&hit, `SELECT COUNT(*) FROM (SELECT ? AS text) AS texts INNER JOIN (SELECT CONCAT('%', ?, '%') AS pattern) AS patterns ON texts.text LIKE patterns.pattern`, comment, ng); err != nil {
						t.Fatal(err)
					}
					matched = matched || hit > 0
				}
				if !matched {
					want = append(want, id)
					remaining++
				}
			}
			for i := range comments {
				want = append(want, int64(i+1001))
			}
			body, _ := json.Marshal(ModerateRequest{NGWord: word})
			resetTestPosts(t, db)
			if rec := improvementRequest(t, e, store, http.MethodPost, "/api/livestream/3/moderate", string(body), 1); rec.Code != http.StatusBadRequest {
				t.Fatalf("non-owner moderation: expected 400, got %d", rec.Code)
			}
			rec := improvementRequest(t, e, store, http.MethodPost, "/api/livestream/1/moderate", string(body), 1)
			if rec.Code != http.StatusCreated {
				t.Fatalf("moderation: status=%d body=%s", rec.Code, rec.Body.String())
			}
			var got []int64
			current := currentMutations.Load()
			current.dataMu.RLock()
			for id := range current.commentsByID {
				got = append(got, id)
			}
			current.dataMu.RUnlock()
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("remaining comments: got=%v want=%v", got, want)
			}
			var count int
			state, _ := current.stream(1)
			state.rowsMu.RLock()
			count = len(state.words)
			state.rowsMu.RUnlock()
			if count != 2 {
				t.Fatalf("NG words after owner/non-owner requests: count=%d", count)
			}
			statsRec := improvementRequest(t, e, store, http.MethodGet, "/api/user/alpha/statistics", "", 1)
			var stats UserStatistics
			if err := json.Unmarshal(statsRec.Body.Bytes(), &stats); err != nil || statsRec.Code != http.StatusOK || stats.TotalLivecomments != remaining || stats.TotalTip != remaining {
				t.Fatalf("statistics after moderation: status=%d stats=%+v remaining=%d err=%v", statsRec.Code, stats, remaining, err)
			}
			if rec := improvementRequest(t, e, store, http.MethodPost, "/api/livestream/1/livecomment", `{"comment":"LEGACY","tip":0}`, 1); rec.Code != http.StatusBadRequest {
				t.Fatalf("post moderated comment: expected 400, got %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	t.Run("incremental_moderation", func(t *testing.T) { checkIncrementalModeration(t, db, e, store) })
	t.Run("durable_posts", func(t *testing.T) { checkDurablePosts(t, db, e, store) })

}

func improvementRequest(t *testing.T, e *echo.Echo, store *struct{}, method, path, body string, userID int64) *httptest.ResponseRecorder {
	t.Helper()
	req := improvementHTTPRequest(t, store, method, path, body, userID)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func improvementHTTPRequest(t *testing.T, store *struct{}, method, path, body string, userID int64) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	value, _, err := encodeSession(userID, time.Now().Add(time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: defaultSessionIDKey, Value: value})

	return req
}
