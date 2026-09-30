package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
)

type PostLivecommentRequest struct {
	Comment string `json:"comment"`
	Tip     int64  `json:"tip"`
}

type LivecommentModel struct {
	ID           int64  `db:"id"`
	UserID       int64  `db:"user_id"`
	LivestreamID int64  `db:"livestream_id"`
	Comment      string `db:"comment"`
	Tip          int64  `db:"tip"`
	CreatedAt    int64  `db:"created_at"`
}

type Livecomment struct {
	ID         int64      `json:"id"`
	User       User       `json:"user"`
	Livestream Livestream `json:"livestream"`
	Comment    string     `json:"comment"`
	Tip        int64      `json:"tip"`
	CreatedAt  int64      `json:"created_at"`
}

type LivecommentReport struct {
	ID          int64       `json:"id"`
	Reporter    User        `json:"reporter"`
	Livecomment Livecomment `json:"livecomment"`
	CreatedAt   int64       `json:"created_at"`
}

type LivecommentReportModel struct {
	ID                  int64  `db:"id"`
	UserID              int64  `db:"user_id"`
	LivestreamID        int64  `db:"livestream_id"`
	LivecommentID       int64  `db:"livecomment_id"`
	CreatedAt           int64  `db:"created_at"`
	CommentUserID       int64  `db:"comment_user_id"`
	CommentLivestreamID int64  `db:"comment_livestream_id"`
	Comment             string `db:"comment"`
	CommentTip          int64  `db:"comment_tip"`
	CommentCreatedAt    int64  `db:"comment_created_at"`
}

type ModerateRequest struct {
	NGWord string `json:"ng_word"`
}

type NGWord struct {
	ID           int64  `json:"id" db:"id"`
	UserID       int64  `json:"user_id" db:"user_id"`
	LivestreamID int64  `json:"livestream_id" db:"livestream_id"`
	Word         string `json:"word" db:"word"`
	CreatedAt    int64  `json:"created_at" db:"created_at"`
}

func getLivecommentsHandler(c echo.Context) error {
	ctx := c.Request().Context()

	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	livestreamID, err := strconv.Atoi(c.Param("livestream_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id in path must be integer")
	}

	query := "SELECT * FROM livecomments WHERE livestream_id = ? ORDER BY created_at DESC"
	if c.QueryParam("limit") != "" {
		limit, err := strconv.Atoi(c.QueryParam("limit"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "limit query parameter must be integer")
		}
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	livecommentModels := []LivecommentModel{}
	err = dbConn.SelectContext(ctx, &livecommentModels, query, livestreamID)
	if errors.Is(err, sql.ErrNoRows) {
		return c.JSON(http.StatusOK, []*Livecomment{})
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livecomments: "+err.Error())
	}

	livecomments, err := fillLivecommentResponses(ctx, nil, livecommentModels)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to fil livecomments: "+err.Error())
	}

	return c.JSON(http.StatusOK, livecomments)
}

func getNgwords(c echo.Context) error {
	ctx := c.Request().Context()

	if err := verifyUserSession(c); err != nil {
		return err
	}

	// error already checked
	sess, _ := session.Get(defaultSessionIDKey, c)
	// existence already checked
	userID := sess.Values[defaultUserIDKey].(int64)

	livestreamID, err := strconv.Atoi(c.Param("livestream_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id in path must be integer")
	}

	var ngWords []*NGWord
	if err := dbConn.SelectContext(ctx, &ngWords, "SELECT * FROM ng_words WHERE user_id = ? AND livestream_id = ? ORDER BY created_at DESC", userID, livestreamID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.JSON(http.StatusOK, []*NGWord{})
		} else {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get NG words: "+err.Error())
		}
	}

	return c.JSON(http.StatusOK, ngWords)
}

func postLivecommentHandler(c echo.Context) error {
	ctx := c.Request().Context()
	if err := verifyUserSession(c); err != nil {
		return err
	}
	livestreamID, err := strconv.ParseInt(c.Param("livestream_id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id in path must be integer")
	}
	sess, _ := session.Get(defaultSessionIDKey, c)
	userID := sess.Values[defaultUserIDKey].(int64)
	var req PostLivecommentRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to decode request")
	}
	ref, err := currentReferences.Load().livestream(ctx, livestreamID)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "livestream not found")
	}
	model := LivecommentModel{UserID: userID, LivestreamID: livestreamID, Comment: req.Comment, Tip: req.Tip, CreatedAt: time.Now().Unix()}
	var response Livecomment
	err = submitMutation(ctx, func(workCtx context.Context, tx *sqlx.Tx) error {
		var spam bool
		query := `SELECT EXISTS(SELECT 1 FROM ng_words WHERE user_id = ? AND livestream_id = ? AND CONVERT(? USING utf8mb4) COLLATE utf8mb4_general_ci LIKE CONCAT('%', word COLLATE utf8mb4_general_ci, '%'))`
		if err := tx.GetContext(workCtx, &spam, query, ref.model.UserID, livestreamID, model.Comment); err != nil {
			return err
		}
		if spam {
			return echo.NewHTTPError(http.StatusBadRequest, "このコメントがスパム判定されました")
		}
		result, err := tx.NamedExecContext(workCtx, "INSERT INTO livecomments (user_id, livestream_id, comment, tip, created_at) VALUES (:user_id, :livestream_id, :comment, :tip, :created_at)", model)
		if err != nil {
			return err
		}
		model.ID, err = result.LastInsertId()
		if err != nil {
			return err
		}
		response, err = fillLivecommentResponse(workCtx, tx, model)
		return err
	})
	if err != nil {
		return mutationHTTPError(err)
	}
	return c.JSON(http.StatusCreated, response)
}

func reportLivecommentHandler(c echo.Context) error {
	ctx := c.Request().Context()
	if err := verifyUserSession(c); err != nil {
		return err
	}
	livestreamID, err := strconv.ParseInt(c.Param("livestream_id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id in path must be integer")
	}
	commentID, err := strconv.ParseInt(c.Param("livecomment_id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livecomment_id in path must be integer")
	}
	sess, _ := session.Get(defaultSessionIDKey, c)
	userID := sess.Values[defaultUserIDKey].(int64)
	if _, err := currentReferences.Load().livestream(ctx, livestreamID); err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "livestream not found")
	}
	var response LivecommentReport
	err = submitMutation(ctx, func(workCtx context.Context, tx *sqlx.Tx) error {
		var comment LivecommentModel
		if err := tx.GetContext(workCtx, &comment, "SELECT * FROM livecomments WHERE id = ?", commentID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return echo.NewHTTPError(http.StatusNotFound, "livecomment not found")
			}
			return err
		}
		model := LivecommentReportModel{UserID: userID, LivestreamID: livestreamID, LivecommentID: commentID, CreatedAt: time.Now().Unix(), CommentUserID: comment.UserID, CommentLivestreamID: comment.LivestreamID, Comment: comment.Comment, CommentTip: comment.Tip, CommentCreatedAt: comment.CreatedAt}
		result, err := tx.NamedExecContext(workCtx, `INSERT INTO livecomment_reports(user_id,livestream_id,livecomment_id,created_at,comment_user_id,comment_livestream_id,comment,comment_tip,comment_created_at) VALUES (:user_id,:livestream_id,:livecomment_id,:created_at,:comment_user_id,:comment_livestream_id,:comment,:comment_tip,:comment_created_at)`, model)
		if err != nil {
			return err
		}
		model.ID, err = result.LastInsertId()
		if err != nil {
			return err
		}
		response, err = fillLivecommentReportResponse(workCtx, tx, model)
		return err
	})
	if err != nil {
		return mutationHTTPError(err)
	}
	return c.JSON(http.StatusCreated, response)
}

func moderateHandler(c echo.Context) error {
	ctx := c.Request().Context()
	if err := verifyUserSession(c); err != nil {
		return err
	}
	livestreamID, err := strconv.ParseInt(c.Param("livestream_id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id in path must be integer")
	}
	sess, _ := session.Get(defaultSessionIDKey, c)
	userID := sess.Values[defaultUserIDKey].(int64)
	var req ModerateRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to decode request")
	}
	ref, err := currentReferences.Load().livestream(ctx, livestreamID)
	if err != nil || ref.model.UserID != userID {
		return echo.NewHTTPError(http.StatusBadRequest, "A streamer can't moderate livestreams that other streamers own")
	}
	var wordID int64
	word := req.NGWord
	err = submitMutation(ctx, func(workCtx context.Context, tx *sqlx.Tx) error {
		result, err := tx.ExecContext(workCtx, "INSERT INTO ng_words(user_id,livestream_id,word,created_at) VALUES (?,?,?,?)", userID, livestreamID, word, time.Now().Unix())
		if err != nil {
			return err
		}
		wordID, err = result.LastInsertId()
		if err != nil {
			return err
		}
		var words []string
		if err := tx.SelectContext(workCtx, &words, "SELECT word FROM ng_words WHERE livestream_id = ?", livestreamID); err != nil {
			return err
		}
		predicates := make([]string, len(words))
		args := []interface{}{livestreamID}
		for i, w := range words {
			predicates[i] = "comment COLLATE utf8mb4_general_ci LIKE CONCAT('%', ?, '%')"
			args = append(args, w)
		}
		_, err = tx.ExecContext(workCtx, "DELETE FROM livecomments WHERE livestream_id = ? AND ("+strings.Join(predicates, " OR ")+")", args...)
		return err
	})
	if err != nil {
		return mutationHTTPError(err)
	}
	return c.JSON(http.StatusCreated, map[string]interface{}{"word_id": wordID})
}

func fillLivecommentResponse(ctx context.Context, tx *sqlx.Tx, model LivecommentModel) (Livecomment, error) {
	responses, err := fillLivecommentResponses(ctx, tx, []LivecommentModel{model})
	if err != nil {
		return Livecomment{}, err
	}
	return responses[0], nil
}

func fillLivecommentReportResponse(ctx context.Context, tx *sqlx.Tx, model LivecommentReportModel) (LivecommentReport, error) {
	responses, err := fillLivecommentReportResponses(ctx, tx, []*LivecommentReportModel{&model})
	if err != nil {
		return LivecommentReport{}, err
	}
	return responses[0], nil
}
