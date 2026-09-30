package main

import (
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"strconv"

	"github.com/labstack/echo/v4"
)

type LivestreamStatistics struct {
	Rank           int64 `json:"rank"`
	ViewersCount   int64 `json:"viewers_count"`
	TotalReactions int64 `json:"total_reactions"`
	TotalReports   int64 `json:"total_reports"`
	MaxTip         int64 `json:"max_tip"`
}

type LivestreamRankingEntry struct {
	LivestreamID   int64 `db:"livestream_id"`
	Score          int64 `db:"score"`
	TotalReactions int64 `db:"total_reactions"`
	MaxTip         int64 `db:"max_tip"`
}
type LivestreamRanking []LivestreamRankingEntry

func (r LivestreamRanking) Len() int      { return len(r) }
func (r LivestreamRanking) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r LivestreamRanking) Less(i, j int) bool {
	if r[i].Score == r[j].Score {
		return r[i].LivestreamID < r[j].LivestreamID
	} else {
		return r[i].Score < r[j].Score
	}
}

type UserStatistics struct {
	Rank              int64  `json:"rank"`
	ViewersCount      int64  `json:"viewers_count"`
	TotalReactions    int64  `json:"total_reactions"`
	TotalLivecomments int64  `json:"total_livecomments"`
	TotalTip          int64  `json:"total_tip"`
	FavoriteEmoji     string `json:"favorite_emoji"`
}

type UserRankingEntry struct {
	Username          string `db:"username"`
	Score             int64  `db:"score"`
	TotalReactions    int64  `db:"total_reactions"`
	TotalLivecomments int64  `db:"total_livecomments"`
	TotalTip          int64  `db:"total_tip"`
}
type UserRanking []UserRankingEntry

func (r UserRanking) Len() int      { return len(r) }
func (r UserRanking) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r UserRanking) Less(i, j int) bool {
	if r[i].Score == r[j].Score {
		return r[i].Username < r[j].Username
	} else {
		return r[i].Score < r[j].Score
	}
}

func getUserStatisticsHandler(c echo.Context) error {
	ctx := c.Request().Context()

	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	username := c.Param("username")
	// ユーザごとに、紐づく配信について、累計リアクション数、累計ライブコメント数、累計売上金額を算出
	// また、現在の合計視聴者数もだす

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	var user UserModel
	if err := tx.GetContext(ctx, &user, "SELECT * FROM users WHERE name = ?", username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusBadRequest, "not found user that has the given username")
		} else {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user: "+err.Error())
		}
	}

	// Aggregate each child table separately to avoid multiplying joined rows.
	var ranking UserRanking
	query := `
	SELECT u.name AS username,
	       COALESCE(r.total_reactions, 0) + COALESCE(lc.total_tip, 0) AS score,
	       COALESCE(r.total_reactions, 0) AS total_reactions,
	       COALESCE(lc.total_livecomments, 0) AS total_livecomments,
	       COALESCE(lc.total_tip, 0) AS total_tip
	FROM users u
	LEFT JOIN (
	    SELECT l.user_id, COUNT(*) AS total_reactions
	    FROM livestreams l
	    INNER JOIN reactions r ON r.livestream_id = l.id
	    GROUP BY l.user_id
	) r ON r.user_id = u.id
	LEFT JOIN (
	    SELECT l.user_id, COUNT(*) AS total_livecomments, SUM(lc.tip) AS total_tip
	    FROM livestreams l
	    INNER JOIN livecomments lc ON lc.livestream_id = l.id
	    GROUP BY l.user_id
	) lc ON lc.user_id = u.id`
	if err := tx.SelectContext(ctx, &ranking, query); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user ranking: "+err.Error())
	}
	sort.Sort(ranking)

	stats := UserStatistics{Rank: 1}
	for i := len(ranking) - 1; i >= 0; i-- {
		entry := ranking[i]
		if entry.Username == username {
			stats.TotalReactions = entry.TotalReactions
			stats.TotalLivecomments = entry.TotalLivecomments
			stats.TotalTip = entry.TotalTip
			break
		}
		stats.Rank++
	}

	query = `SELECT COUNT(*)
	FROM livestreams l
	INNER JOIN livestream_viewers_history h ON h.livestream_id = l.id
	WHERE l.user_id = ?`
	if err := tx.GetContext(ctx, &stats.ViewersCount, query, user.ID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to count user viewers: "+err.Error())
	}

	// お気に入り絵文字
	var favoriteEmoji string
	query = `
	SELECT r.emoji_name
	FROM users u
	INNER JOIN livestreams l ON l.user_id = u.id
	INNER JOIN reactions r ON r.livestream_id = l.id
	WHERE u.name = ?
	GROUP BY emoji_name
	ORDER BY COUNT(*) DESC, emoji_name DESC
	LIMIT 1
	`
	if err := tx.GetContext(ctx, &favoriteEmoji, query, username); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to find favorite emoji: "+err.Error())
	}

	stats.FavoriteEmoji = favoriteEmoji
	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}
	return c.JSON(http.StatusOK, stats)
}

func getLivestreamStatisticsHandler(c echo.Context) error {
	ctx := c.Request().Context()

	if err := verifyUserSession(c); err != nil {
		return err
	}

	id, err := strconv.Atoi(c.Param("livestream_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id in path must be integer")
	}
	livestreamID := int64(id)

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	var livestream LivestreamModel
	if err := tx.GetContext(ctx, &livestream, "SELECT * FROM livestreams WHERE id = ?", livestreamID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusBadRequest, "cannot get stats of not found livestream")
		} else {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livestream: "+err.Error())
		}
	}

	var ranking LivestreamRanking
	query := `
	SELECT l.id AS livestream_id,
	       COALESCE(r.total_reactions, 0) + COALESCE(lc.total_tip, 0) AS score,
	       COALESCE(r.total_reactions, 0) AS total_reactions,
	       COALESCE(lc.max_tip, 0) AS max_tip
	FROM livestreams l
	LEFT JOIN (
	    SELECT livestream_id, COUNT(*) AS total_reactions
	    FROM reactions GROUP BY livestream_id
	) r ON r.livestream_id = l.id
	LEFT JOIN (
	    SELECT livestream_id, SUM(tip) AS total_tip, MAX(tip) AS max_tip
	    FROM livecomments GROUP BY livestream_id
	) lc ON lc.livestream_id = l.id`
	if err := tx.SelectContext(ctx, &ranking, query); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livestream ranking: "+err.Error())
	}
	sort.Sort(ranking)

	stats := LivestreamStatistics{Rank: 1}
	for i := len(ranking) - 1; i >= 0; i-- {
		entry := ranking[i]
		if entry.LivestreamID == livestreamID {
			stats.TotalReactions = entry.TotalReactions
			stats.MaxTip = entry.MaxTip
			break
		}
		stats.Rank++
	}

	// 視聴者数算出
	var viewersCount int64
	if err := tx.GetContext(ctx, &viewersCount, `SELECT COUNT(*) FROM livestreams l INNER JOIN livestream_viewers_history h ON h.livestream_id = l.id WHERE l.id = ?`, livestreamID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to count livestream viewers: "+err.Error())
	}

	// スパム報告数
	var totalReports int64
	if err := tx.GetContext(ctx, &totalReports, `SELECT COUNT(*) FROM livestreams l INNER JOIN livecomment_reports r ON r.livestream_id = l.id WHERE l.id = ?`, livestreamID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to count total spam reports: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	stats.ViewersCount = viewersCount
	stats.TotalReports = totalReports
	return c.JSON(http.StatusOK, stats)
}
