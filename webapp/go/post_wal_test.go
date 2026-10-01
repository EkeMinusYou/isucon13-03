package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

func TestPostFrames(t *testing.T) {
	records := []postRecord{{Seq: 1, Kind: "comment", ID: 1, UserID: 2, StreamID: 3, Text: "quote'\\😀", Tip: 7, CreatedAt: 123}, {Seq: 2, Kind: "reaction", ID: 1, Text: "emoji"}}
	frame, err := encodePostFrame(records)
	if err != nil {
		t.Fatal(err)
	}
	got, size, err := readPostFrame(bytes.NewReader(frame), 0)
	if err != nil || size != int64(len(frame)) || fmt.Sprint(got) != fmt.Sprint(records) {
		t.Fatalf("roundtrip: %v %d %v", got, size, err)
	}
	for _, cut := range []int{1, 7, 8, postFrameHeaderSize - 1, postFrameHeaderSize, len(frame) - 1} {
		if _, _, err := readPostFrame(bytes.NewReader(frame[:cut]), 0); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("partial frame at %d: %v", cut, err)
		}
	}
	bad := append([]byte{}, frame...)
	bad[len(bad)-1] ^= 1
	if _, _, err := readPostFrame(bytes.NewReader(bad), 0); err == nil {
		t.Fatal("corrupted frame accepted")
	}
	if _, _, err := readPostFrame(bytes.NewReader(frame), 1); err == nil {
		t.Fatal("sequence gap accepted")
	}
}

func TestPreallocatedPostFrames(t *testing.T) {
	frame, err := encodePostFrame([]postRecord{{Seq: 1, Kind: "reaction", ID: 1, StreamID: 1, Text: "preallocated"}})
	if err != nil {
		t.Fatal(err)
	}
	// Model every prefix write into the already initialized area, including
	// complete headers followed by partial payloads and completion footers.
	for cut := 0; cut < len(frame); cut++ {
		area := make([]byte, len(frame)+postEndSize)
		copy(area, frame[:cut])
		_, _, err := readPostFrame(bytes.NewReader(area), 0)
		if err == nil && bytes.Equal(area[:len(frame)], frame) {
			// An unwritten zero suffix can already equal the complete footer.
			continue
		}
		if cut == 0 {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("unused area: %v", err)
			}
		} else if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, errPostFrameHeader) && !errors.Is(err, io.EOF) {
			t.Fatalf("incomplete prefix %d: %v", cut, err)
		}
	}
	bad := append([]byte(nil), frame...)
	bad[postFrameHeaderSize] ^= 1
	if _, _, err := readPostFrame(bytes.NewReader(bad), 0); err == nil || errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("complete corrupted frame treated as an incomplete tail: %v", err)
	}
}

func writerTestStore(t *testing.T) *postStore {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "posts")
	if err != nil {
		t.Fatal(err)
	}
	const capacity = 1 << 20
	if err := initializePostFile(context.Background(), file, "00000000-0000-0000-0000-000000000000", capacity); err != nil {
		t.Fatal(err)
	}
	s := &postStore{file: file, capacity: capacity, syncFile: func() error { return syncPostData(file) }, streams: make(map[int64]*postStream), slots: make(chan struct{}, 256), requests: make(chan postRequest, 256), nextComment: 1, nextReaction: 1, nextWord: 1, nextReport: 1}
	s.durableBytes.Store(int64(postHeaderSize))
	s.publishedBytes.Store(int64(postHeaderSize))
	t.Cleanup(func() { close(s.requests); s.writers.Wait(); file.Close() })
	return s
}

func TestPostSyncPublicationAndCancellation(t *testing.T) {
	s := writerTestStore(t)
	started, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(release) }) })
	s.syncFile = func() error { close(started); <-release; return s.file.Sync() }
	s.writers.Add(1)
	go s.writePosts()
	ctx, cancel := context.WithCancel(context.Background())
	model := &ReactionModel{UserID: 1, LivestreamID: 1, EmojiName: "first"}
	done := make(chan error, 1)
	go func() { done <- s.submit(ctx, 1, &reactionMutation{model: model}) }()
	<-started
	cancel()
	select {
	case err := <-done:
		t.Fatalf("returned before Sync: %v", err)
	default:
	}
	if s.durableSeq.Load() != 0 {
		t.Fatal("unsynchronized frame published")
	}
	readCtx, stop := context.WithTimeout(context.Background(), 5*time.Millisecond)
	_, _, err := s.timeline(readCtx, 1, -1, false)
	stop()
	if err != nil {
		t.Fatalf("read of committed state blocked on Sync: %v", err)
	}
	unblock.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_, rows, err := s.timeline(context.Background(), 1, -1, false)
	if err != nil || len(rows) != 1 || rows[0].ID != model.ID || model.ID <= 0 {
		t.Fatalf("durable publication: %+v %v", rows, err)
	}
}

func TestPostSyncWakeClosure(t *testing.T) {
	for _, closeWake := range []bool{false, true} {
		t.Run(fmt.Sprintf("closed=%t", closeWake), func(t *testing.T) {
			s := writerTestStore(t)
			s.writtenBytes = s.durableBytes.Load()
			s.syncWake = make(chan struct{}, 1)
			r := postRequest{record: postRecord{Kind: "reaction", StreamID: 1, Text: "drain"}, done: make(chan postResult, 1)}
			s.appendPostBatch([]postRequest{r})
			if closeWake {
				close(s.syncWake)
			}
			done := make(chan struct{})
			go func() { defer close(done); s.syncPosts() }()
			defer func() {
				if !closeWake {
					close(s.syncWake)
				}
				<-done
			}()
			select {
			case result := <-r.done:
				if result.err != nil || result.record.ID != 1 {
					t.Fatalf("drain result: %+v", result)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("notification or shutdown left a frame pending")
			}
			if s.syncs.Load() != 1 || s.singleRecordSyncs.Load() != 1 || s.durableSeq.Load() != 1 {
				t.Fatal("singleton was not durably published")
			}
		})
	}
}

func TestPostSyncFailureAndGrouping(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		s := writerTestStore(t)
		s.syncFile = func() error { return errors.New("injected Sync failure") }
		s.writers.Add(1)
		go s.writePosts()
		model := &ReactionModel{LivestreamID: 1, EmojiName: "never-acknowledged"}
		if err := s.submit(context.Background(), 1, &reactionMutation{model: model}); err == nil || model.ID != 0 {
			t.Fatal("failed Sync acknowledged")
		}
		if _, _, err := s.timeline(context.Background(), 1, -1, false); err == nil {
			t.Fatal("failed engine served timeline")
		}
	})
	t.Run("grouping", func(t *testing.T) {
		s := writerTestStore(t)
		batch := make([]postRequest, 16)
		for i := range batch {
			batch[i] = postRequest{record: postRecord{Kind: "reaction", UserID: 1, StreamID: int64(i + 1), Text: "grouped"}, done: make(chan postResult, 1)}
		}
		for _, r := range batch {
			s.requests <- r
		}
		s.writers.Add(1)
		go s.writePosts()
		for i, r := range batch {
			result := <-r.done
			if result.err != nil || result.record.ID != int64(i+1) {
				t.Fatalf("group result: %+v", result)
			}
		}
		if s.syncs.Load() != 1 || s.records.Load() != 16 {
			t.Fatal("independent streams did not share Sync")
		}
	})
}

func TestPostAppendOverlapsSyncWithoutEarlyAcknowledgement(t *testing.T) {
	s := writerTestStore(t)
	started := make(chan int, 2)
	releases := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var once [2]sync.Once
	t.Cleanup(func() {
		for i := range once {
			once[i].Do(func() { close(releases[i]) })
		}
	})
	var calls atomic.Int64
	s.syncFile = func() error {
		i := int(calls.Add(1) - 1)
		started <- i
		<-releases[i]
		return s.file.Sync()
	}
	s.writers.Add(1)
	go s.writePosts()
	first, second := &ReactionModel{LivestreamID: 1, EmojiName: "first"}, &ReactionModel{LivestreamID: 2, EmojiName: "second"}
	done1, done2 := make(chan error, 1), make(chan error, 1)
	go func() { done1 <- s.submit(context.Background(), 1, &reactionMutation{model: first}) }()
	if i := <-started; i != 0 {
		t.Fatal("unexpected first Sync")
	}
	go func() { done2 <- s.submit(context.Background(), 2, &reactionMutation{model: second}) }()
	deadline := time.Now().Add(time.Second)
	for {
		s.walMu.Lock()
		pending := len(s.pending)
		s.walMu.Unlock()
		if pending != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("append blocked on previous Sync")
		}
		time.Sleep(time.Millisecond)
	}
	once[0].Do(func() { close(releases[0]) })
	if err := <-done1; err != nil {
		t.Fatal(err)
	}
	if i := <-started; i != 1 {
		t.Fatal("unexpected second Sync")
	}
	if s.durableSeq.Load() != 1 {
		t.Fatal("first Sync acknowledged a later frame")
	}
	select {
	case err := <-done2:
		t.Fatalf("second frame acknowledged early: %v", err)
	default:
	}
	_, rows, err := s.timeline(context.Background(), 2, -1, false)
	if err != nil || len(rows) != 0 {
		t.Fatal("unsynchronized later frame became visible")
	}
	once[1].Do(func() { close(releases[1]) })
	if err := <-done2; err != nil {
		t.Fatal(err)
	}
	if second.ID <= first.ID || s.durableSeq.Load() != 2 {
		t.Fatal("sequence or IDs lost")
	}
}

func TestPostPreallocatedCapacity(t *testing.T) {
	s := writerTestStore(t)
	frame, err := encodePostFrame([]postRecord{{Seq: 1, ID: 1, Kind: "reaction", StreamID: 1, Text: "fits"}})
	if err != nil {
		t.Fatal(err)
	}
	s.capacity = int64(postHeaderSize + len(frame) + postEndSize)
	s.writers.Add(1)
	go s.writePosts()
	first := &ReactionModel{LivestreamID: 1, EmojiName: "fits"}
	if err := s.submit(context.Background(), 1, &reactionMutation{model: first}); err != nil {
		t.Fatal(err)
	}
	if err := s.submit(context.Background(), 2, &reactionMutation{model: &ReactionModel{LivestreamID: 2, EmojiName: "full"}}); err == nil {
		t.Fatal("WAL end marker overwritten when capacity was exhausted")
	}
	info, err := s.file.Stat()
	if err != nil || info.Size() != 1<<20 {
		t.Fatalf("preallocated file changed size: %v %v", info, err)
	}
	if err := checkPostZeroTail(s.file, s.durableBytes.Load(), info.Size()); err != nil {
		t.Fatal(err)
	}
}

func resetTestPosts(t *testing.T, db *sqlx.DB) string {
	t.Helper()
	if old := currentMutations.Swap(nil); old != nil {
		_ = old.close()
	}
	if _, err := db.Exec("UPDATE post_wal_state SET ready=FALSE WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := createPostGenerationCapacity(context.Background(), db, dir, 1<<20); err != nil {
		t.Fatal(err)
	}
	s, err := openPostStore(context.Background(), db, dir)
	if err != nil {
		t.Fatal(err)
	}
	currentMutations.Store(s)
	return dir
}

func checkDurablePosts(t *testing.T, db *sqlx.DB, e *echo.Echo, cookies *struct{}) {
	ctx := context.Background()
	clearRows := func() {
		if old := currentMutations.Swap(nil); old != nil {
			_ = old.close()
		}
		if _, err := db.Exec("DELETE FROM ng_words; DELETE FROM livecomments; DELETE FROM reactions; DELETE FROM livecomment_reports"); err != nil {
			t.Fatal(err)
		}
	}
	e.GET("/api/payment", GetPaymentResult)
	t.Run("preallocated_tail_recovery_and_corruption", func(t *testing.T) {
		for _, name := range []string{"partial_header", "partial_payload", "partial_footer", "header_corruption", "payload_corruption", "data_after_end", "data_after_incomplete", "truncated_file"} {
			t.Run(name, func(t *testing.T) {
				clearRows()
				dir := resetTestPosts(t, db)
				s := currentMutations.Load()
				posted := &ReactionModel{UserID: 2, LivestreamID: 1, EmojiName: "acknowledged"}
				if err := s.submit(ctx, 1, &reactionMutation{model: posted}); err != nil {
					t.Fatal(err)
				}
				if err := s.close(); err != nil {
					t.Fatal(err)
				}
				end := s.durableBytes.Load()
				frame, err := encodePostFrame([]postRecord{{Seq: 2, ID: posted.ID + 1, Kind: "reaction", UserID: 2, StreamID: 1, Text: "unfinished"}})
				if err != nil {
					t.Fatal(err)
				}
				file, err := os.OpenFile(filepath.Join(dir, s.generation+".wal"), os.O_RDWR, 0640)
				if err != nil {
					t.Fatal(err)
				}
				data := append([]byte(nil), frame...)
				position := end
				expectError := false
				switch name {
				case "partial_header":
					data = data[:3]
				case "partial_payload":
					data = data[:postFrameHeaderSize+7]
				case "partial_footer", "data_after_incomplete":
					data = data[:len(data)-1]
					if name == "data_after_incomplete" {
						if _, err := file.WriteAt(frame, end+int64(len(frame))); err != nil {
							t.Fatal(err)
						}
						expectError = true
					}
				case "header_corruption":
					data[0] ^= 1
					expectError = true
				case "payload_corruption":
					data[postFrameHeaderSize] ^= 1
					expectError = true
				case "data_after_end":
					position += postEndSize
					expectError = true
				case "truncated_file":
					if err := file.Truncate(s.capacity - 1); err != nil {
						t.Fatal(err)
					}
					data = nil
					expectError = true
				}
				if _, err := file.WriteAt(data, position); err != nil {
					t.Fatal(err)
				}
				if err := file.Sync(); err != nil {
					t.Fatal(err)
				}
				file.Close()
				recovered, err := openPostStore(ctx, db, dir)
				if expectError {
					if err == nil {
						recovered.close()
						t.Fatal("corrupted or missing durable data was accepted")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				currentMutations.Store(recovered)
				if recovered.durableSeq.Load() != 1 || recovered.durableBytes.Load() != end {
					t.Fatal("incomplete tail was replayed")
				}
				if err := checkPostZeroTail(recovered.file, end, recovered.capacity); err != nil {
					t.Fatal(err)
				}
				info, err := recovered.file.Stat()
				if err != nil || info.Size() != s.capacity {
					t.Fatalf("recovery removed preallocation: %v %v", info, err)
				}
				_, rows, err := recovered.timeline(ctx, 1, -1, false)
				if err != nil || len(rows) != 1 || rows[0].ID != posted.ID || rows[0].EmojiName != posted.EmojiName {
					t.Fatalf("acknowledged post lost: %+v %v", rows, err)
				}
				next := &ReactionModel{UserID: 2, LivestreamID: 1, EmojiName: "after-tail-recovery"}
				if err := recovered.submit(ctx, 1, &reactionMutation{model: next}); err != nil || next.ID <= posted.ID {
					t.Fatalf("cannot append after recovery: %+v %v", next, err)
				}
			})
		}
	})
	t.Run("concurrent_posts_reads_moderation_and_restart", func(t *testing.T) {
		clearRows()
		dir := resetTestPosts(t, db)
		s := currentMutations.Load()
		beforeViewers, err := s.livestreamStatistics(1)
		if err != nil {
			t.Fatal(err)
		}
		state, _ := s.stream(1)
		state.rowsMu.RLock()
		previousViewerRows := state.viewers[2]
		state.rowsMu.RUnlock()
		for i := 0; i < 2; i++ {
			if err := s.submit(ctx, 1, &viewerMutation{userID: 2, livestreamID: 1, enter: true}); err != nil {
				t.Fatal(err)
			}
		}
		afterEnter, err := s.livestreamStatistics(1)
		if err != nil || afterEnter.ViewersCount != beforeViewers.ViewersCount+2 {
			t.Fatalf("duplicate viewer entries: %+v %v", afterEnter, err)
		}
		if err := s.submit(ctx, 1, &viewerMutation{userID: 2, livestreamID: 1}); err != nil {
			t.Fatal(err)
		}
		afterExit, err := s.livestreamStatistics(1)
		if err != nil || afterExit.ViewersCount != beforeViewers.ViewersCount-previousViewerRows {
			t.Fatalf("viewer exit did not delete all entries: %+v %v", afterExit, err)
		}
		if err := s.submit(ctx, 1, &viewerMutation{userID: 2, livestreamID: 1, enter: true}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var idsMu sync.Mutex
		ids := map[int64]bool{}
		for stream := int64(1); stream <= 3; stream++ {
			wg.Add(1)
			go func(stream int64) {
				defer wg.Done()
				for i := 0; i < 8; i++ {
					model := &LivecommentModel{UserID: 2, LivestreamID: stream, Comment: fmt.Sprintf("post-%d-%d", stream, i), Tip: 7, CreatedAt: 123}
					if err := s.submit(ctx, stream, &commentMutation{model: model, ownerID: 1}); err != nil {
						t.Error(err)
						return
					}
					idsMu.Lock()
					if ids[model.ID] {
						t.Error("duplicate post ID")
					}
					ids[model.ID] = true
					idsMu.Unlock()
					rows, _, err := s.timeline(ctx, stream, 1, true)
					if err != nil || len(rows) != 1 || rows[0].ID != model.ID {
						t.Errorf("immediate read: %+v %v", rows, err)
					}
				}
			}(stream)
		}
		wg.Wait()
		s.dataMu.RLock()
		total := len(s.commentsByID)
		s.dataMu.RUnlock()
		if total != 24 {
			t.Fatalf("durable memory posts: %d", total)
		}
		var persisted int
		if err := db.Get(&persisted, "SELECT COUNT(*) FROM livecomments"); err != nil || persisted != 0 {
			t.Fatalf("hot rows leaked into MySQL: %d %v", persisted, err)
		}
		rec := improvementRequest(t, e, cookies, http.MethodPost, "/api/livestream/1/livecomment", `{"comment":"moderation-target","tip":11}`, 2)
		if rec.Code != 201 {
			t.Fatal(rec.Body)
		}
		var posted Livecomment
		if err := json.Unmarshal(rec.Body.Bytes(), &posted); err != nil {
			t.Fatal(err)
		}
		path := fmt.Sprintf("/api/livestream/1/livecomment/%d/report", posted.ID)
		if rec := improvementRequest(t, e, cookies, http.MethodPost, path, "", 2); rec.Code != 201 {
			t.Fatal(rec.Body)
		}
		if rec := improvementRequest(t, e, cookies, http.MethodPost, "/api/livestream/1/moderate", `{"ng_word":"moderation-target"}`, 1); rec.Code != 201 {
			t.Fatal(rec.Body)
		}
		if rec := improvementRequest(t, e, cookies, http.MethodPost, "/api/livestream/1/livecomment", `{"comment":"MODERATION-TARGET","tip":11}`, 2); rec.Code != 400 {
			t.Fatalf("NG predicate: %d %s", rec.Code, rec.Body)
		}
		if rec := improvementRequest(t, e, cookies, http.MethodPost, path, "", 2); rec.Code != 404 {
			t.Fatalf("deleted report: %d", rec.Code)
		}
		read := improvementRequest(t, e, cookies, http.MethodGet, "/api/livestream/1/livecomment", "", 1)
		if read.Code != 200 || strings.Contains(read.Body.String(), "moderation-target") {
			t.Fatalf("deleted memory row: %s", read.Body)
		}
		payment := improvementRequest(t, e, cookies, http.MethodGet, "/api/payment", "", 1)
		var amount PaymentResult
		if err := json.Unmarshal(payment.Body.Bytes(), &amount); err != nil || payment.Code != 200 || amount.TotalTip != 168 {
			t.Fatalf("payment after deletion: %s %v", payment.Body, err)
		}
		beforeUser, err := s.userStatistics(1)
		if err != nil {
			t.Fatal(err)
		}
		beforeStream, err := s.livestreamStatistics(1)
		if err != nil {
			t.Fatal(err)
		}
		state.rowsMu.RLock()
		beforeWord := state.words[len(state.words)-1]
		beforeReport := state.reports[len(state.reports)-1]
		state.rowsMu.RUnlock()
		if err := s.close(); err != nil {
			t.Fatal(err)
		}
		// A partial trailing frame cannot resurrect a durable deletion.
		file, err := os.OpenFile(filepath.Join(dir, s.generation+".wal"), os.O_WRONLY, 0640)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteAt([]byte{1, 2, 3}, s.durableBytes.Load()); err != nil {
			t.Fatal(err)
		}
		file.Close()
		recovered, err := openPostStore(ctx, db, dir)
		if err != nil {
			t.Fatal(err)
		}
		currentMutations.Store(recovered)
		afterUser, err := recovered.userStatistics(1)
		if err != nil || afterUser != beforeUser {
			t.Fatalf("recovered user statistics: %+v %+v %v", afterUser, beforeUser, err)
		}
		afterStream, err := recovered.livestreamStatistics(1)
		if err != nil || afterStream != beforeStream {
			t.Fatalf("recovered stream statistics: %+v %+v %v", afterStream, beforeStream, err)
		}
		recoveredState, _ := recovered.stream(1)
		recoveredState.rowsMu.RLock()
		afterWord := recoveredState.words[len(recoveredState.words)-1]
		afterReport := recoveredState.reports[len(recoveredState.reports)-1]
		recoveredState.rowsMu.RUnlock()
		if afterWord != beforeWord || afterReport != beforeReport {
			t.Fatal("NG word ID or report snapshot changed after replay")
		}
		totalTip, err := recovered.payment()
		if err != nil || totalTip != 168 {
			t.Fatalf("recovered payment: %d %v", totalTip, err)
		}
		read = improvementRequest(t, e, cookies, http.MethodGet, "/api/livestream/1/livecomment", "", 1)
		if read.Code != 200 || strings.Contains(read.Body.String(), "moderation-target") {
			t.Fatalf("deletion resurrected: %s", read.Body)
		}
		next := &LivecommentModel{UserID: 2, LivestreamID: 1, Comment: "after-restart", Tip: 3}
		if err := recovered.submit(ctx, 1, &commentMutation{model: next, ownerID: 1}); err != nil || next.ID <= posted.ID {
			t.Fatalf("ID reused after deletion/recovery: %d %v", next.ID, err)
		}
		reports := improvementRequest(t, e, cookies, http.MethodGet, "/api/livestream/1/report", "", 1)
		if reports.Code != 200 || !strings.Contains(reports.Body.String(), "moderation-target") {
			t.Fatal("report snapshot lost")
		}
	})
	t.Run("wal_is_authoritative", func(t *testing.T) {
		clearRows()
		dir := resetTestPosts(t, db)
		s := currentMutations.Load()
		if _, err := db.Exec("CREATE TRIGGER reject_hot_write BEFORE INSERT ON reactions FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='hot SQL writes forbidden'"); err != nil {
			t.Fatal(err)
		}
		defer db.Exec("DROP TRIGGER reject_hot_write")
		model := &ReactionModel{UserID: 2, LivestreamID: 1, EmojiName: "wal-only"}
		if err := s.submit(ctx, 1, &reactionMutation{model: model}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := s.close(); err != nil {
				t.Fatal(err)
			}
			recovered, err := openPostStore(ctx, db, dir)
			if err != nil {
				t.Fatal(err)
			}
			s = recovered
			currentMutations.Store(s)
			_, rows, err := s.timeline(ctx, 1, -1, false)
			if err != nil || len(rows) != 1 || rows[0] != *model {
				t.Fatalf("WAL replay: %+v %v", rows, err)
			}
			var count int
			if err := db.Get(&count, "SELECT COUNT(*) FROM reactions"); err != nil || count != 0 {
				t.Fatalf("WAL wrote SQL rows: %d %v", count, err)
			}
		}
	})
	t.Run("unready_generation", func(t *testing.T) {
		s := currentMutations.Swap(nil)
		dir := filepath.Dir(s.file.Name())
		if err := s.close(); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("UPDATE post_wal_state SET ready=FALSE WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		if recovered, err := openPostStore(ctx, db, dir); err != nil || recovered != nil {
			t.Fatalf("incomplete initialization resumed: %v %v", recovered, err)
		}
		if _, err := db.Exec("DELETE FROM livecomments; DELETE FROM reactions"); err != nil {
			t.Fatal(err)
		}
		gen, err := createPostGenerationCapacity(ctx, db, dir, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if gen == s.generation {
			t.Fatal("initialization reused generation")
		}
		recovered, err := openPostStore(ctx, db, dir)
		if err != nil {
			t.Fatal(err)
		}
		currentMutations.Store(recovered)
		comments, reactions, err := recovered.timeline(ctx, 1, -1, true)
		if err != nil || len(comments) != 0 || len(reactions) != 0 {
			t.Fatalf("old generation replayed: %+v %+v %v", comments, reactions, err)
		}
		_, reactions, err = recovered.timeline(ctx, 1, -1, false)
		if err != nil || len(reactions) != 0 {
			t.Fatalf("old reaction replayed: %+v %v", reactions, err)
		}
		if err := removeOldPostGenerations(dir, gen); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(s.file.Name()); !os.IsNotExist(err) {
			t.Fatalf("old generation retained: %v", err)
		}
	})
}
