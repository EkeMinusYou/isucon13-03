package main

import (
	"github.com/EkeMinusYou/isucon-template/tools/measurement/app"
	"github.com/labstack/echo/v4"
)

func startMeasurement() error {
	_, err := app.Start(app.Options{
		MaxProfileSeconds: 180,
		Pools:             []app.Pool{{DB: dbConn, DatabaseHost: "127.0.0.1", Name: "isupipe", Role: "primary"}},
		Snapshot:          mutationSnapshot,
	})
	return err
}

func mutationSnapshot() app.Snapshot {
	var posts map[string]any
	metrics := make(map[string]any)
	if store := currentMutations.Load(); store != nil {
		addCacheMetrics(metrics, "ng_cache", &store.ngResults)
		posts = store.metrics()
		metrics["wal_records_total"] = posts["records"]
		metrics["wal_syncs_total"] = posts["syncs"]
		metrics["wal_sync_ns_total"] = posts["sync_ns"]
		metrics["wal_single_record_syncs_total"] = posts["single_record_syncs"]
		metrics["wal_pending_bytes"] = posts["pending_bytes"]
		metrics["wal_bytes"] = posts["wal_bytes"]
		metrics["wal_published_bytes"] = posts["published_bytes"]
		metrics["wal_durable_seq"] = posts["durable_seq"]
		metrics["wal_published_seq"] = posts["published_seq"]
		metrics["icon_reuses_total"] = posts["icon_reuses"]
		metrics["icon_uploads_total"] = posts["icon_uploads"]
		metrics["icon_wal_bytes_total"] = posts["icon_bytes"]
		metrics["icon_wal_chunks_total"] = posts["icon_chunks"]
	}
	if refs := currentReferences.Load(); refs != nil {
		addCacheMetrics(metrics, "row_json_cache", &refs.rowJSON)
	}
	return app.Snapshot{Metrics: metrics, Extra: map[string]any{"post_wal": posts}}
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
