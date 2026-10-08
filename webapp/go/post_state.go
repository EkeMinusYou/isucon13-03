package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type postTotals struct {
	comments, reactions, tip, viewers, reports int64
	maxTip                                     int64
	tips                                       map[int64]int64
	emojis                                     map[string]int64
	emojiNames                                 map[string]string
}

func newPostTotals() *postTotals {
	return &postTotals{tips: make(map[int64]int64), emojis: make(map[string]int64), emojiNames: make(map[string]string)}
}
func (s *postStore) initState() {
	if s.commentsByID == nil {
		s.commentsByID = make(map[int64]LivecommentModel)
		s.userNames = make(map[int64]string)
		s.owners = make(map[int64]int64)
		s.streamTotals = make(map[int64]*postTotals)
		s.userTotals = make(map[int64]*postTotals)
	}
}
func (s *postStore) addUser(id int64, name string) {
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	s.initState()
	s.userNames[id] = name
	if s.userTotals[id] == nil {
		s.userTotals[id] = newPostTotals()
	}
}
func (s *postStore) addStream(id, owner int64) {
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	s.initState()
	if _, ok := s.owners[id]; ok {
		return
	}
	s.owners[id] = owner
	if s.streamTotals[id] == nil {
		s.streamTotals[id] = newPostTotals()
	}
	if s.userTotals[owner] == nil {
		s.userTotals[owner] = newPostTotals()
	}
	// Viewer requests may precede reservation of this ID, as in the SQL implementation.
	s.userTotals[owner].viewers += s.streamTotals[id].viewers
}

// Publication runs in WAL order in the writer, after each complete file write.
func (s *postStore) publishRecords(records []postRecord, reserve bool) error {
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	s.initState()
	for _, r := range records {
		if r.Kind == "icon" {
			if err := s.publishIconChunk(r); err != nil {
				return err
			}
			continue
		}
		state, err := s.stream(r.StreamID)
		if err != nil {
			return err
		}
		if reserve && r.cost() > 0 {
			if err := s.reserveRow(r.cost()); err != nil {
				return err
			}
		}
		t := s.streamTotals[r.StreamID]
		if t == nil {
			t = newPostTotals()
			s.streamTotals[r.StreamID] = t
		}
		owner := s.owners[r.StreamID]
		u := s.userTotals[owner]
		if u == nil {
			u = newPostTotals()
			s.userTotals[owner] = u
		}
		state.rowsMu.Lock()
		switch r.Kind {
		case "comment":
			if r.Seq == 0 {
				state.ngCleanOwner = 0
			}
			row := r.comment()
			if _, exists := s.commentsByID[row.ID]; exists {
				state.rowsMu.Unlock()
				return fmt.Errorf("duplicate WAL comment %d", row.ID)
			}
			s.commentsByID[row.ID] = row
			i := sort.Search(len(state.comments), func(i int) bool {
				x := state.comments[i]
				return x.CreatedAt < row.CreatedAt || (x.CreatedAt == row.CreatedAt && x.ID < row.ID)
			})
			state.comments = append(state.comments, LivecommentModel{})
			copy(state.comments[i+1:], state.comments[i:])
			state.comments[i] = row
			t.comments++
			t.tip += r.Tip
			u.comments++
			u.tip += r.Tip
			s.totalTip += r.Tip
			if t.comments == 1 || r.Tip > t.maxTip {
				t.maxTip = r.Tip
			}
			t.tips[r.Tip]++
		case "reaction":
			row := r.reaction()
			i := sort.Search(len(state.reactions), func(i int) bool {
				x := state.reactions[i]
				return x.CreatedAt < row.CreatedAt || (x.CreatedAt == row.CreatedAt && x.ID < row.ID)
			})
			state.reactions = append(state.reactions, ReactionModel{})
			copy(state.reactions[i+1:], state.reactions[i:])
			state.reactions[i] = row
			t.reactions++
			u.reactions++
			// utf8mb4_bin groups VARCHAR values with trailing spaces ignored.
			key := strings.TrimRight(r.Text, " ")
			if u.emojis[key] == 0 {
				u.emojiNames[key] = r.Text
			}
			u.emojis[key]++
		case "moderate":
			state.words = append(state.words, NGWord{ID: r.ID, UserID: r.UserID, LivestreamID: r.StreamID, Word: r.Text, CreatedAt: r.CreatedAt})
			if state.ngPatterns == nil {
				state.ngPatterns = make(map[int64][]*ngPattern)
			}
			pattern := compileNG(r.Text)
			state.ngPatterns[r.UserID] = append(state.ngPatterns[r.UserID], pattern)
			state.allPatterns = append(state.allPatterns, pattern)
			deleted := make(map[int64]bool, len(r.Deleted))
			for _, id := range r.Deleted {
				row, exists := s.commentsByID[id]
				if !exists || row.LivestreamID != r.StreamID {
					state.rowsMu.Unlock()
					return fmt.Errorf("invalid WAL deletion %d", id)
				}
				if deleted[id] {
					state.rowsMu.Unlock()
					return fmt.Errorf("duplicate WAL deletion %d", id)
				}
				deleted[id] = true
				delete(s.commentsByID, id)
				t.comments--
				t.tip -= row.Tip
				u.comments--
				u.tip -= row.Tip
				s.totalTip -= row.Tip
				t.tips[row.Tip]--
				if t.tips[row.Tip] == 0 {
					delete(t.tips, row.Tip)
				}
				s.releaseRow(commentRecord(row).cost())
			}
			if len(deleted) > 0 {
				remaining := make([]LivecommentModel, 0, len(state.comments)-len(deleted))
				for _, row := range state.comments {
					if !deleted[row.ID] {
						remaining = append(remaining, row)
					}
				}
				state.comments = remaining
				t.maxTip = 0
				first := true
				for tip := range t.tips {
					if first || tip > t.maxTip {
						t.maxTip = tip
						first = false
					}
				}
			}
			// SQL seed rows (Seq == 0) were not filtered. WAL moderation records
			// were prepared under the stream gate, including records from before
			// this optimization. Replay can therefore restore the same invariant.
			state.ngCleanOwner = 0
			if r.Seq > 0 && owner != 0 && r.UserID == owner && len(state.ngPatterns) == 1 {
				state.ngCleanOwner = owner
			}
		case "report":
			if r.Report == nil {
				state.rowsMu.Unlock()
				return fmt.Errorf("missing report snapshot")
			}
			state.reports = append(state.reports, *r.Report)
			t.reports++
		case "enter":
			if state.viewers == nil {
				state.viewers = make(map[int64]int64)
			}
			state.viewers[r.UserID]++
			t.viewers++
			u.viewers++
		case "exit":
			n := state.viewers[r.UserID]
			delete(state.viewers, r.UserID)
			t.viewers -= n
			u.viewers -= n
			for i := int64(0); i < n; i++ {
				s.releaseRow(256)
			}
		default:
			state.rowsMu.Unlock()
			return fmt.Errorf("unknown WAL kind %q", r.Kind)
		}
		state.rowsMu.Unlock()
	}
	return nil
}

func (s *postStore) load(ctx context.Context) error {
	var users []struct {
		ID   int64  `db:"id"`
		Name string `db:"name"`
	}
	if err := s.db.SelectContext(ctx, &users, "SELECT id,name FROM users"); err != nil {
		return err
	}
	for _, u := range users {
		s.addUser(u.ID, u.Name)
	}
	var streams []struct {
		ID    int64 `db:"id"`
		Owner int64 `db:"user_id"`
	}
	if err := s.db.SelectContext(ctx, &streams, "SELECT id,user_id FROM livestreams"); err != nil {
		return err
	}
	for _, x := range streams {
		if _, err := s.stream(x.ID); err != nil {
			return err
		}
		s.addStream(x.ID, x.Owner)
	}
	var comments []LivecommentModel
	if err := s.db.SelectContext(ctx, &comments, "SELECT * FROM livecomments ORDER BY id"); err != nil {
		return err
	}
	for _, row := range comments {
		r := commentRecord(row)
		if err := s.publishRecords([]postRecord{r}, true); err != nil {
			return err
		}
		if row.ID >= s.nextComment {
			s.nextComment = row.ID + 1
		}
	}
	var reactions []ReactionModel
	if err := s.db.SelectContext(ctx, &reactions, "SELECT * FROM reactions ORDER BY id"); err != nil {
		return err
	}
	for _, row := range reactions {
		r := postRecord{Kind: "reaction", ID: row.ID, UserID: row.UserID, StreamID: row.LivestreamID, Text: row.EmojiName, CreatedAt: row.CreatedAt}
		if err := s.publishRecords([]postRecord{r}, true); err != nil {
			return err
		}
		if row.ID >= s.nextReaction {
			s.nextReaction = row.ID + 1
		}
	}
	var words []NGWord
	if err := s.db.SelectContext(ctx, &words, "SELECT * FROM ng_words ORDER BY id"); err != nil {
		return err
	}
	for _, row := range words {
		r := postRecord{Kind: "moderate", ID: row.ID, UserID: row.UserID, StreamID: row.LivestreamID, Text: row.Word, CreatedAt: row.CreatedAt}
		if err := s.publishRecords([]postRecord{r}, true); err != nil {
			return err
		}
		if row.ID >= s.nextWord {
			s.nextWord = row.ID + 1
		}
	}
	var reports []LivecommentReportModel
	if err := s.db.SelectContext(ctx, &reports, "SELECT * FROM livecomment_reports ORDER BY id"); err != nil {
		return err
	}
	for _, row := range reports {
		r := postRecord{Kind: "report", ID: row.ID, UserID: row.UserID, StreamID: row.LivestreamID, Report: &row, CreatedAt: row.CreatedAt}
		if err := s.publishRecords([]postRecord{r}, true); err != nil {
			return err
		}
		if row.ID >= s.nextReport {
			s.nextReport = row.ID + 1
		}
	}
	var viewers []LivestreamViewerModel
	if err := s.db.SelectContext(ctx, &viewers, "SELECT user_id,livestream_id,created_at FROM livestream_viewers_history ORDER BY id"); err != nil {
		return err
	}
	for _, row := range viewers {
		r := postRecord{Kind: "enter", UserID: row.UserID, StreamID: row.LivestreamID, CreatedAt: row.CreatedAt}
		if err := s.publishRecords([]postRecord{r}, true); err != nil {
			return err
		}
	}
	var counters []struct {
		Name string `db:"TABLE_NAME"`
		Next int64  `db:"next_id"`
	}
	if err := s.db.SelectContext(ctx, &counters, "SELECT TABLE_NAME,COALESCE(AUTO_INCREMENT,1) AS next_id FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME IN ('livecomments','reactions','ng_words','livecomment_reports')"); err != nil {
		return err
	}
	for _, c := range counters {
		kind := map[string]string{"livecomments": "comment", "reactions": "reaction", "ng_words": "moderate", "livecomment_reports": "report"}[c.Name]
		counter := s.idCounter(kind)
		if c.Next > *counter {
			*counter = c.Next
		}
	}
	return nil
}

func (s *postStore) userStatistics(id int64) (UserStatistics, error) {
	if err := s.check(); err != nil {
		return UserStatistics{}, err
	}
	s.dataMu.RLock()
	defer s.dataMu.RUnlock()
	t := s.userTotals[id]
	if t == nil {
		t = newPostTotals()
	}
	result := UserStatistics{Rank: 1, ViewersCount: t.viewers, TotalReactions: t.reactions, TotalLivecomments: t.comments, TotalTip: t.tip}
	score := t.reactions + t.tip
	for other, name := range s.userNames {
		x := s.userTotals[other]
		amount := int64(0)
		if x != nil {
			amount = x.reactions + x.tip
		}
		if amount > score || (amount == score && name > s.userNames[id]) {
			result.Rank++
		}
	}
	best := int64(0)
	key := ""
	for emoji, n := range t.emojis {
		if n > best || (n == best && emoji > key) {
			best = n
			key = emoji
			result.FavoriteEmoji = t.emojiNames[emoji]
		}
	}
	return result, nil
}
func (s *postStore) livestreamStatistics(id int64) (LivestreamStatistics, error) {
	if err := s.check(); err != nil {
		return LivestreamStatistics{}, err
	}
	s.dataMu.RLock()
	defer s.dataMu.RUnlock()
	t := s.streamTotals[id]
	if t == nil {
		t = newPostTotals()
	}
	result := LivestreamStatistics{Rank: 1, ViewersCount: t.viewers, TotalReactions: t.reactions, TotalReports: t.reports, MaxTip: t.maxTip}
	score := t.reactions + t.tip
	for other := range s.owners {
		x := s.streamTotals[other]
		amount := int64(0)
		if x != nil {
			amount = x.reactions + x.tip
		}
		if amount > score || (amount == score && other > id) {
			result.Rank++
		}
	}
	return result, nil
}
func (s *postStore) payment() (int64, error) {
	if err := s.check(); err != nil {
		return 0, err
	}
	s.dataMu.RLock()
	defer s.dataMu.RUnlock()
	return s.totalTip, nil
}
