package main

import (
	"context"
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

func (s *postStore) prepareModeration(ctx context.Context, state *postStream, op *moderationMutation) (postRecord, error) {
	if !utf8.ValidString(op.word) || utf8.RuneCountInString(op.word) > 255 {
		return postRecord{}, echo.NewHTTPError(http.StatusInternalServerError, "NG word exceeds VARCHAR(255)")
	}
	if err := ctx.Err(); err != nil {
		return postRecord{}, err
	}
	state.rowsMu.RLock()
	defer state.rowsMu.RUnlock()
	patterns := []*ngPattern{compileNG(op.word)}
	// Initial rows and foreign authors' words still require a full scan.
	if state.ngCleanOwner == 0 || state.ngCleanOwner != op.userID {
		patterns = append(patterns, state.allPatterns...)
	}
	var deleted []int64
	for i, row := range state.comments {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return postRecord{}, err
			}
		}
		if matchesNG(row.Comment, patterns) {
			deleted = append(deleted, row.ID)
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
