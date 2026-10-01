package main

import (
	"database/sql"
	"errors"
	"github.com/labstack/echo/v4"
	"net/http"
	"strconv"
)

type LivestreamStatistics struct {
	Rank           int64 `json:"rank"`
	ViewersCount   int64 `json:"viewers_count"`
	TotalReactions int64 `json:"total_reactions"`
	TotalReports   int64 `json:"total_reports"`
	MaxTip         int64 `json:"max_tip"`
}
type UserStatistics struct {
	Rank              int64  `json:"rank"`
	ViewersCount      int64  `json:"viewers_count"`
	TotalReactions    int64  `json:"total_reactions"`
	TotalLivecomments int64  `json:"total_livecomments"`
	TotalTip          int64  `json:"total_tip"`
	FavoriteEmoji     string `json:"favorite_emoji"`
}

func getUserStatisticsHandler(c echo.Context) error {
	if err := verifyUserSession(c); err != nil {
		return err
	}
	user, err := currentReferences.Load().namedUser(c.Request().Context(), c.Param("username"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusBadRequest, "not found user that has the given username")
		}
		return err
	}
	store := currentMutations.Load()
	if store == nil {
		return postsUnavailable("initialization is required")
	}
	stats, err := store.userStatistics(user.ID)
	if err != nil {
		return mutationHTTPError(err)
	}
	return c.JSON(http.StatusOK, stats)
}
func getLivestreamStatisticsHandler(c echo.Context) error {
	if err := verifyUserSession(c); err != nil {
		return err
	}
	id, err := strconv.ParseInt(c.Param("livestream_id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id in path must be integer")
	}
	if _, err := currentReferences.Load().livestream(c.Request().Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusBadRequest, "cannot get stats of not found livestream")
		}
		return err
	}
	store := currentMutations.Load()
	if store == nil {
		return postsUnavailable("initialization is required")
	}
	stats, err := store.livestreamStatistics(id)
	if err != nil {
		return mutationHTTPError(err)
	}
	return c.JSON(http.StatusOK, stats)
}
