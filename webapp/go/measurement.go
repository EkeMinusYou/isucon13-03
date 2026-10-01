package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

var measurementStarted = time.Now()

func measurementHandler(maxSeconds int) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", profileHandler(maxSeconds))
	mux.HandleFunc("/debug/sql-pools", func(w http.ResponseWriter, r *http.Request) {
		stats := dbConn.Stats()
		var posts map[string]any
		metrics := make(map[string]any)
		if store := currentMutations.Load(); store != nil {
			posts = store.metrics()
			metrics["wal_records_total"] = posts["records"]
			metrics["wal_syncs_total"] = posts["syncs"]
			metrics["wal_sync_ns_total"] = posts["sync_ns"]
			metrics["wal_single_record_syncs_total"] = posts["single_record_syncs"]
			metrics["wal_pending_bytes"] = posts["pending_bytes"]
			metrics["wal_bytes"] = posts["wal_bytes"]
			metrics["wal_durable_seq"] = posts["durable_seq"]
			metrics["wal_published_seq"] = posts["published_seq"]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":          1,
			"started_unix_ms":  measurementStarted.UnixMilli(),
			"captured_unix_ms": time.Now().UnixMilli(),
			"post_wal":         posts,
			"metrics":          metrics,
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
		c.Response().Before(func() {
			sess, err := requestSession(c)
			if err == nil {
				c.Response().Header().Set("X-Measurement-Session-ID", sess.ID)
			}
		})
		return next(c)
	}
}
