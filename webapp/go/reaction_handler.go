package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
)

type ReactionModel struct {
	ID           int64  `db:"id"`
	EmojiName    string `db:"emoji_name"`
	UserID       int64  `db:"user_id"`
	LivestreamID int64  `db:"livestream_id"`
	CreatedAt    int64  `db:"created_at"`
}

type Reaction struct {
	ID         int64      `json:"id"`
	EmojiName  string     `json:"emoji_name"`
	User       User       `json:"user"`
	Livestream Livestream `json:"livestream"`
	CreatedAt  int64      `json:"created_at"`
}

type PostReactionRequest struct {
	EmojiName string `json:"emoji_name"`
}

func getReactionsHandler(c echo.Context) error {
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
			return echo.NewHTTPError(http.StatusNotFound, "failed to get reactions")
		}
	}

	store := currentMutations.Load()
	if store == nil {
		return postsUnavailable("initialization is required")
	}
	_, reactionModels, err := store.timeline(ctx, int64(livestreamID), limit, false)
	if err != nil {
		return mutationHTTPError(err)
	}

	reactions, err := reactionResponseJSON(ctx, reactionModels)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill reaction: "+err.Error())
	}

	defer releaseResponseJSON(reactions)
	return writeCompressedResponseJSON(c, reactions)
}

func postReactionHandler(c echo.Context) error {
	ctx := c.Request().Context()
	if err := verifyUserSession(c); err != nil {
		return err
	}
	livestreamID, err := strconv.ParseInt(c.Param("livestream_id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id in path must be integer")
	}
	userID := sessionUserID(c)
	var req PostReactionRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to decode request")
	}
	model := ReactionModel{UserID: userID, LivestreamID: livestreamID, EmojiName: req.EmojiName, CreatedAt: time.Now().Unix()}
	response := newResponseJSONBuilder(ctx)
	_, err = response.user(userID)
	if err == nil {
		_, err = response.streamReference(livestreamID)
	}
	if err != nil {
		return mutationHTTPError(err)
	}
	err = submitMutation(ctx, livestreamID, &reactionMutation{model: &model})
	if err != nil {
		return mutationHTTPError(err)
	}
	clear(response.users)
	clear(response.streams)
	data, err := response.reactionJSON(model)
	if err != nil {
		return mutationHTTPError(err)
	}
	return writeCreatedResponseJSON(c, data)
}
