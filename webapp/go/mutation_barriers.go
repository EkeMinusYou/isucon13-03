package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
)

type moderationMutation struct {
	userID, livestreamID int64
	word                 string
	wordID               *int64
}

func (*moderationMutation) mutationOperation() {}
func (op *moderationMutation) apply(ctx context.Context, tx *sqlx.Tx) error {
	result, err := tx.ExecContext(ctx, "INSERT INTO ng_words(user_id,livestream_id,word,created_at) VALUES (?,?,?,?)", op.userID, op.livestreamID, op.word, time.Now().Unix())
	if err != nil {
		return err
	}
	*op.wordID, err = result.LastInsertId()
	if err != nil {
		return err
	}
	var words []string
	if err := tx.SelectContext(ctx, &words, "SELECT word FROM ng_words WHERE livestream_id = ?", op.livestreamID); err != nil {
		return err
	}
	predicates := make([]string, len(words))
	args := []interface{}{op.livestreamID}
	for i, word := range words {
		predicates[i] = "comment COLLATE utf8mb4_general_ci LIKE CONCAT('%', ?, '%')"
		args = append(args, word)
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM livecomments WHERE livestream_id = ? AND ("+strings.Join(predicates, " OR ")+")", args...)
	return err
}

type reportMutation struct {
	userID, livestreamID, commentID int64
	response                        *LivecommentReport
}

func (*reportMutation) mutationOperation() {}
func (op *reportMutation) apply(ctx context.Context, tx *sqlx.Tx) error {
	var comment LivecommentModel
	if err := tx.GetContext(ctx, &comment, "SELECT * FROM livecomments WHERE id = ?", op.commentID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "livecomment not found")
		}
		return err
	}
	model := LivecommentReportModel{UserID: op.userID, LivestreamID: op.livestreamID, LivecommentID: op.commentID, CreatedAt: time.Now().Unix(), CommentUserID: comment.UserID, CommentLivestreamID: comment.LivestreamID, Comment: comment.Comment, CommentTip: comment.Tip, CommentCreatedAt: comment.CreatedAt}
	response, err := fillLivecommentReportResponse(ctx, tx, model)
	if err != nil {
		return err
	}
	result, err := tx.NamedExecContext(ctx, `INSERT INTO livecomment_reports(user_id,livestream_id,livecomment_id,created_at,comment_user_id,comment_livestream_id,comment,comment_tip,comment_created_at) VALUES (:user_id,:livestream_id,:livecomment_id,:created_at,:comment_user_id,:comment_livestream_id,:comment,:comment_tip,:comment_created_at)`, model)
	if err != nil {
		return err
	}
	response.ID, err = result.LastInsertId()
	if err != nil {
		return err
	}
	*op.response = response
	return nil
}

type viewerMutation struct {
	userID, livestreamID int64
	enter                bool
}

func (*viewerMutation) mutationOperation() {}
func (op *viewerMutation) apply(ctx context.Context, tx *sqlx.Tx) error {
	if op.enter {
		_, err := tx.ExecContext(ctx, "INSERT INTO livestream_viewers_history (user_id,livestream_id,created_at) VALUES (?,?,?)", op.userID, op.livestreamID, time.Now().Unix())
		return err
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM livestream_viewers_history WHERE user_id = ? AND livestream_id = ?", op.userID, op.livestreamID)
	return err
}
