package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
)

var measurementStarted = time.Now()

func measurementHandler(maxSeconds int) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", profileHandler(maxSeconds))
	mux.HandleFunc("/debug/sql-pools", func(w http.ResponseWriter, r *http.Request) {
		stats := dbConn.Stats()
		var posts map[string]any
		if store := currentMutations.Load(); store != nil {
			posts = store.metrics()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":          1,
			"started_unix_ms":  measurementStarted.UnixMilli(),
			"captured_unix_ms": time.Now().UnixMilli(),
			"post_wal":         posts,
			"pools": []map[string]any{{
				"database_host": "127.0.0.1", "name": "isupipe", "role": "primary", "shard": 0,
				"max_open_connections": stats.MaxOpenConnections,
				"open_connections":     stats.OpenConnections, "in_use": stats.InUse, "idle": stats.Idle,
				"wait_count": stats.WaitCount, "wait_duration_ns": stats.WaitDuration.Nanoseconds(),
				"max_idle_closed": stats.MaxIdleClosed, "max_idle_time_closed": stats.MaxIdleTimeClosed,
				"max_lifetime_closed": stats.MaxLifetimeClosed,
			}},
		})
	})
	return mux
}

func measurementSession(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		// Before runs at header commit, after login has updated the session.
		c.Response().Before(func() {
			sess, err := session.Get(defaultSessionIDKey, c)
			if err != nil {
				return
			}
			id, ok := sess.Values[defaultSessionIDKey].(string)
			if !ok || id == "" {
				return
			}
			mac := hmac.New(sha256.New, secret)
			_, _ = mac.Write([]byte("measurement-session:" + id))
			c.Response().Header().Set("X-Measurement-Session-ID", hex.EncodeToString(mac.Sum(nil)))
		})
		return next(c)
	}
}
