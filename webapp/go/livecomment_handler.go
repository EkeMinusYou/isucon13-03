package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
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

	limit := -1
	if c.QueryParam("limit") != "" {
		limit, err = strconv.Atoi(c.QueryParam("limit"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "limit query parameter must be integer")
		}
		if limit < 0 {
			return echo.NewHTTPError(http.StatusInternalServerError, "invalid negative limit")
		}
	}

	store := currentMutations.Load()
	if store == nil {
		return postsUnavailable("initialization is required")
	}
	livecommentModels, _, err := store.timeline(ctx, int64(livestreamID), limit, true)
	if err != nil {
		return mutationHTTPError(err)
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
	response, err := fillLivecommentResponse(ctx, nil, model)
	if err != nil {
		return mutationHTTPError(err)
	}
	err = submitMutation(ctx, livestreamID, &commentMutation{model: &model, ownerID: ref.model.UserID})
	if err != nil {
		return mutationHTTPError(err)
	}
	response.ID = model.ID
	refreshResponseIcons(&response.User, &response.Livestream.Owner)
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
	err = submitMutation(ctx, livestreamID, &reportMutation{userID: userID, livestreamID: livestreamID, commentID: commentID, response: &response})
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
	err = submitMutation(ctx, livestreamID, &moderationMutation{userID: userID, livestreamID: livestreamID, word: word, wordID: &wordID})
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
