package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jmoiron/sqlx"
)

func checkBulkMutations(t *testing.T, db *sqlx.DB) {
	ctx := context.Background()
	w := currentMutations.Load()
	request := func(op mutationOperation) mutationRequest {
		return mutationRequest{ctx: ctx, op: op, done: make(chan error, 1)}
	}
	comment := func(stream int64, text string) *commentMutation {
		return &commentMutation{ownerID: 1, model: &LivecommentModel{UserID: 2, LivestreamID: stream, Comment: text, Tip: 7, CreatedAt: 123}}
	}
	reaction := func(stream int64, emoji string) *reactionMutation {
		return &reactionMutation{model: &ReactionModel{UserID: 2, LivestreamID: stream, EmojiName: emoji, CreatedAt: 456}}
	}
	t.Run("concurrent_mixed_batches_id_mapping", func(t *testing.T) {
		if _, err := db.Exec("DELETE FROM ng_words; DELETE FROM livecomments; DELETE FROM reactions"); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for stream := int64(1); stream <= 4; stream++ {
			wg.Add(1)
			go func(stream int64) {
				defer wg.Done()
				batch := make([]mutationRequest, 0, 32)
				for i := 0; i < 16; i++ {
					batch = append(batch, request(comment(stream, fmt.Sprintf("bulk-%d-%d-'\\😀", stream, i))), request(reaction(stream, fmt.Sprintf("emoji-%d-%d", stream, i))))
				}
				w.write(batch)
				var previousComment, previousReaction int64
				for _, r := range batch {
					if err := <-r.done; err != nil {
						t.Error(err)
						return
					}
					switch op := r.op.(type) {
					case *commentMutation:
						var saved LivecommentModel
						if err := db.Get(&saved, "SELECT * FROM livecomments WHERE id=?", op.model.ID); err != nil || saved != *op.model {
							t.Errorf("comment ID mapped to wrong row: %+v %+v %v", saved, op.model, err)
						}
						if previousComment > 0 && op.model.ID != previousComment+1 {
							t.Error("comment IDs are not consecutive within a VALUES insert")
						}
						previousComment = op.model.ID
					case *reactionMutation:
						var saved ReactionModel
						if err := db.Get(&saved, "SELECT * FROM reactions WHERE id=?", op.model.ID); err != nil || saved != *op.model {
							t.Errorf("reaction ID mapped to wrong row: %+v %+v %v", saved, op.model, err)
						}
						if previousReaction > 0 && op.model.ID != previousReaction+1 {
							t.Error("reaction IDs are not consecutive within a VALUES insert")
						}
						previousReaction = op.model.ID
					}
				}
			}(stream)
		}
		wg.Wait()
	})
	t.Run("spam_bad_rows_and_cancelled_request_are_isolated", func(t *testing.T) {
		if _, err := db.Exec("DELETE FROM ng_words; DELETE FROM livecomments; DELETE FROM reactions; INSERT INTO ng_words(user_id,livestream_id,word,created_at) VALUES (1,1,'café',0),(1,1,'a_b',0),(1,1,'q%r',0),(1,1,'slash\\\\_',0)"); err != nil {
			t.Fatal(err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		batch := []mutationRequest{
			request(comment(1, "healthy-before")), request(comment(1, "CAFE")),
			request(comment(1, strings.Repeat("界", 256))), request(reaction(1, "healthy-reaction")),
			request(reaction(1, strings.Repeat("x", 256))), request(comment(1, "healthy-after")),
			request(comment(1, "aXb")), request(comment(1, "qZZr")), request(comment(1, "slash_")),
			{ctx: cancelled, op: comment(1, "cancelled"), done: make(chan error, 1)},
		}
		w.write(batch)
		for i, r := range batch {
			err := <-r.done
			wantError := i != 0 && i != 3 && i != 5
			if (err != nil) != wantError {
				t.Errorf("request %d: %v", i, err)
			}
		}
		var texts []string
		if err := db.Select(&texts, "SELECT comment FROM livecomments ORDER BY id"); err != nil || fmt.Sprint(texts) != "[healthy-before healthy-after]" {
			t.Fatalf("partial batch corrupted healthy comments: %v %v", texts, err)
		}
		var count int
		if err := db.Get(&count, "SELECT COUNT(*) FROM reactions WHERE emoji_name='healthy-reaction'"); err != nil || count != 1 {
			t.Fatalf("healthy reaction lost: %d %v", count, err)
		}
	})
	t.Run("post_moderate_post_and_report_barriers", func(t *testing.T) {
		if _, err := db.Exec("DELETE FROM ng_words; DELETE FROM livecomments; DELETE FROM livecomment_reports"); err != nil {
			t.Fatal(err)
		}
		old := comment(1, "bulk-barrier-word")
		var wordID int64
		batch := []mutationRequest{
			request(old), request(&moderationMutation{userID: 1, livestreamID: 1, word: "bulk-barrier-word", wordID: &wordID}),
			request(comment(1, "bulk-barrier-word")), request(comment(1, "safe-after-barrier")),
		}
		w.write(batch)
		for i, r := range batch {
			if err := <-r.done; (err != nil) != (i == 2) {
				t.Fatalf("moderation boundary request %d: %v", i, err)
			}
		}
		var count int
		if err := db.Get(&count, "SELECT COUNT(*) FROM livecomments WHERE comment='bulk-barrier-word'"); err != nil || count != 0 {
			t.Fatalf("moderation was reordered: %d %v", count, err)
		}
		posted := comment(1, "report-barrier-word")
		first := request(posted)
		w.write([]mutationRequest{first})
		if err := <-first.done; err != nil {
			t.Fatal(err)
		}
		var report LivecommentReport
		batch = []mutationRequest{
			request(comment(1, "before-report")),
			request(&reportMutation{userID: 2, livestreamID: 1, commentID: posted.model.ID, response: &report}),
			request(&moderationMutation{userID: 1, livestreamID: 1, word: "report-barrier-word", wordID: &wordID}),
			request(&reportMutation{userID: 2, livestreamID: 1, commentID: posted.model.ID, response: &LivecommentReport{}}),
			request(comment(1, "after-report")),
		}
		w.write(batch)
		for i, r := range batch {
			if err := <-r.done; (err != nil) != (i == 3) {
				t.Fatalf("report boundary request %d: %v", i, err)
			}
		}
		if report.Livecomment.ID != posted.model.ID || report.Livecomment.Comment != posted.model.Comment || report.Livecomment.Tip != posted.model.Tip {
			t.Fatalf("report snapshot differs: %+v", report)
		}
		var saved LivecommentReportModel
		if err := db.Get(&saved, "SELECT * FROM livecomment_reports WHERE id=?", report.ID); err != nil || saved.Comment != posted.model.Comment {
			t.Fatalf("report snapshot not durable: %+v %v", saved, err)
		}
	})
	t.Run("commit_failure_rejects_all_bulk_results", func(t *testing.T) {
		first, second := comment(1, "commit-failure-1"), reaction(1, "commit-failure-2")
		batch := []mutationRequest{request(first), request(second), request(mutationFunc(func(ctx context.Context, tx *sqlx.Tx) error {
			var id int64
			if err := tx.GetContext(ctx, &id, "SELECT CONNECTION_ID()"); err != nil {
				return err
			}
			_, err := db.Exec(fmt.Sprintf("KILL CONNECTION %d", id))
			return err
		}))}
		w.write(batch)
		for i, r := range batch {
			if err := <-r.done; err == nil {
				t.Errorf("uncommitted request %d acknowledged", i)
			}
		}
		var count int
		if err := db.Get(&count, "SELECT COUNT(*) FROM livecomments WHERE comment='commit-failure-1'"); err != nil || count != 0 {
			t.Fatalf("failed batch retained comment: %d %v", count, err)
		}
		if err := db.Get(&count, "SELECT COUNT(*) FROM reactions WHERE emoji_name='commit-failure-2'"); err != nil || count != 0 {
			t.Fatalf("failed batch retained reaction: %d %v", count, err)
		}
	})
}
