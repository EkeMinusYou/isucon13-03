package main

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

func checkIncrementalModeration(t *testing.T, db *sqlx.DB, e *echo.Echo, cookies *struct{}) {
	for _, foreign := range []bool{false, true} {
		name := "owner_words"
		if foreign {
			name = "foreign_words"
		}
		t.Run(name, func(t *testing.T) {
			if old := currentMutations.Swap(nil); old != nil {
				if err := old.close(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`DELETE FROM ng_words; DELETE FROM livecomments; DELETE FROM reactions; DELETE FROM livecomment_reports;
INSERT INTO ng_words (user_id,livestream_id,word,created_at) VALUES (1,1,'café',0);
INSERT INTO livecomments (id,user_id,livestream_id,comment,tip,created_at) VALUES
 (2001,2,1,'CAFE',7,0), (2002,2,1,'aXb',11,0),
 (2003,2,1,'slash_',13,0), (2004,2,1,'legacy-other',17,0), (2005,2,1,'safe',19,0)`); err != nil {
				t.Fatal(err)
			}
			if foreign {
				if _, err := db.Exec("INSERT INTO ng_words (user_id,livestream_id,word,created_at) VALUES (2,1,'legacy-other',0)"); err != nil {
					t.Fatal(err)
				}
			}
			dir := resetTestPosts(t, db)
			// The native LIKE expression is independent of the optimized JSON_TABLE
			// query and remains the oracle for case, accents, wildcards and escaping.
			matches := func(text string, words []string) bool {
				for _, word := range words {
					var hit bool
					if err := db.Get(&hit, "SELECT ? LIKE CONCAT('%', ?, '%')", text, word); err != nil {
						t.Fatal(err)
					}
					if hit {
						return true
					}
				}
				return false
			}
			ownerWords := []string{"café"}
			allWords := append([]string{}, ownerWords...)
			if foreign {
				allWords = append(allWords, "legacy-other")
			}
			moderate := func(word string) {
				allWords = append(allWords, word)
				ownerWords = append(ownerWords, word)
				s := currentMutations.Load()
				rows, _, err := s.timeline(context.Background(), 1, -1, true)
				if err != nil {
					t.Fatal(err)
				}
				want := []LivecommentModel{}
				var tip int64
				for _, row := range rows {
					if !matches(row.Comment, allWords) {
						want = append(want, row)
						tip += row.Tip
					}
				}
				body, _ := json.Marshal(ModerateRequest{NGWord: word})
				if rec := improvementRequest(t, e, cookies, http.MethodPost, "/api/livestream/1/moderate", string(body), 1); rec.Code != 201 {
					t.Fatalf("moderation: %d %s", rec.Code, rec.Body)
				}
				got, _, err := s.timeline(context.Background(), 1, -1, true)
				gotTip, tipErr := s.payment()
				if err != nil || tipErr != nil || !reflect.DeepEqual(got, want) || gotTip != tip {
					t.Fatalf("moderation differs from full LIKE scan: got=%+v want=%+v tip=%d wantTip=%d errors=%v/%v", got, want, gotTip, tip, err, tipErr)
				}
			}
			post := func(text string) {
				wantStatus := http.StatusCreated
				if matches(text, ownerWords) {
					wantStatus = http.StatusBadRequest
				}
				body, _ := json.Marshal(PostLivecommentRequest{Comment: text, Tip: 3})
				for repeat := 0; repeat < 2; repeat++ {
					if rec := improvementRequest(t, e, cookies, http.MethodPost, "/api/livestream/1/livecomment", string(body), 2); rec.Code != wantStatus {
						t.Fatalf("cached NG predicate for %q: got=%d want=%d body=%s", text, rec.Code, wantStatus, rec.Body)
					}
				}
			}
			moderate("a_b") // Must also remove SQL-seeded CAFE on the first scan.
			for _, text := range []string{"CAFE", "aXb", "legacy-other", "new safe", "slash_"} {
				post(text)
			}
			moderate(`slash\_`)
			post("slash_") // A previously accepted cache entry must not survive the new predicate.
			cacheHits := uint64(0)
			for i := range currentMutations.Load().ngResults {
				hits, _, _ := currentMutations.Load().ngResults[i].stats()
				cacheHits += hits
			}
			if cacheHits < 6 {
				t.Fatalf("NG results were not reused: hits=%d", cacheHits)
			}
			before, _, err := currentMutations.Load().timeline(context.Background(), 1, -1, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := currentMutations.Load().close(); err != nil {
				t.Fatal(err)
			}
			recovered, err := openPostStore(context.Background(), db, dir)
			if err != nil {
				t.Fatal(err)
			}
			currentMutations.Store(recovered)
			after, _, err := recovered.timeline(context.Background(), 1, -1, true)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("replay changed comments: before=%+v after=%+v err=%v", before, after, err)
			}
			for _, text := range []string{"café", "a_b", "slash_", "legacy-other", "qZZr", "😀safe"} {
				post(text)
			}
			moderate("q%r") // Foreign-author patterns must still run after replay.
		})
	}
}
