package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"
)

type moderationMutation struct {
	userID, livestreamID int64
	word                 string
	wordID               *int64
}

func (*moderationMutation) mutationOperation() {}

type reportMutation struct {
	userID, livestreamID, commentID int64
	response                        *LivecommentReport
}

func (*reportMutation) mutationOperation() {}

type viewerMutation struct {
	userID, livestreamID int64
	enter                bool
}

func (*viewerMutation) mutationOperation() {}

// MySQL evaluates only parameter values, preserving LIKE and collation semantics
// without reading a lagging projection or performing a durable database write.
func (s *postStore) matchesNG(ctx context.Context, text, wordsJSON string) (bool, error) {
	if wordsJSON == "" {
		return false, nil
	}
	var hit bool
	err := s.db.GetContext(ctx, &hit, `SELECT EXISTS(SELECT 1 FROM JSON_TABLE(?, '$[*]' COLUMNS(word VARCHAR(255) CHARACTER SET utf8mb4 PATH '$')) AS w WHERE CONVERT(? USING utf8mb4) COLLATE utf8mb4_general_ci LIKE CONCAT('%', w.word COLLATE utf8mb4_general_ci, '%'))`, wordsJSON, text)
	return hit, err
}
func (s *postStore) prepareModeration(ctx context.Context, state *postStream, op *moderationMutation) (postRecord, error) {
	if !utf8.ValidString(op.word) || utf8.RuneCountInString(op.word) > 255 {
		return postRecord{}, echo.NewHTTPError(http.StatusInternalServerError, "NG word exceeds VARCHAR(255)")
	}
	state.rowsMu.RLock()
	words := []string{op.word}
	// Initial SQL rows have no cleanliness guarantee. Foreign authors' words
	// also require full scans because new comments only check the owner's words.
	if state.ngCleanOwner == 0 || state.ngCleanOwner != op.userID {
		words = make([]string, 0, len(state.words)+1)
		for _, word := range state.words {
			words = append(words, word.Word)
		}
		words = append(words, op.word)
	}
	type candidate struct {
		ID   int64  `json:"id"`
		Text string `json:"text"`
	}
	rows := make([]candidate, 0, len(state.comments))
	for _, row := range state.comments {
		rows = append(rows, candidate{row.ID, row.Comment})
	}
	state.rowsMu.RUnlock()
	var deleted []int64
	if len(rows) > 0 {
		rowJSON, err := json.Marshal(rows)
		if err != nil {
			return postRecord{}, err
		}
		wordJSON, err := json.Marshal(words)
		if err != nil {
			return postRecord{}, err
		}
		err = s.db.SelectContext(ctx, &deleted, `SELECT c.id FROM JSON_TABLE(?, '$[*]' COLUMNS(id BIGINT PATH '$.id', text VARCHAR(255) CHARACTER SET utf8mb4 PATH '$.text')) AS c WHERE EXISTS(SELECT 1 FROM JSON_TABLE(?, '$[*]' COLUMNS(word VARCHAR(255) CHARACTER SET utf8mb4 PATH '$')) AS w WHERE c.text COLLATE utf8mb4_general_ci LIKE CONCAT('%',w.word COLLATE utf8mb4_general_ci,'%'))`, string(rowJSON), string(wordJSON))
		if err != nil {
			return postRecord{}, err
		}
	}
	return postRecord{Kind: "moderate", UserID: op.userID, StreamID: op.livestreamID, Text: op.word, Deleted: deleted, CreatedAt: time.Now().Unix()}, nil
}
func (s *postStore) prepareReport(ctx context.Context, op *reportMutation) (postRecord, error) {
	s.dataMu.RLock()
	comment, exists := s.commentsByID[op.commentID]
	s.dataMu.RUnlock()
	if !exists {
		return postRecord{}, echo.NewHTTPError(http.StatusNotFound, "livecomment not found")
	}
	model := LivecommentReportModel{UserID: op.userID, LivestreamID: op.livestreamID, LivecommentID: op.commentID, CreatedAt: time.Now().Unix(), CommentUserID: comment.UserID, CommentLivestreamID: comment.LivestreamID, Comment: comment.Comment, CommentTip: comment.Tip, CommentCreatedAt: comment.CreatedAt}
	response, err := fillLivecommentReportResponse(ctx, nil, model)
	if err != nil {
		return postRecord{}, err
	}
	*op.response = response
	return postRecord{Kind: "report", UserID: op.userID, StreamID: op.livestreamID, CreatedAt: model.CreatedAt, Report: &model}, nil
}
