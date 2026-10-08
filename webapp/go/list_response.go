package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/labstack/echo/v4"
)

const listResponseBudget = 8 << 20 // Per shard; 128 MiB across all shards.

type listResponseKey struct {
	kind         uint8 // 0: all streams; 1: tag; 2: owner.
	id           int64
	limit, count int
	pretty       bool
	tags         *tagCatalog
}

type listIconDependency struct {
	icon *iconEntry
	hash string
}

type completedListResponse struct {
	plain, gzip []byte
	icons       []listIconDependency
	cost        int
}

func (r *completedListResponse) valid() bool {
	for _, dep := range r.icons {
		if dep.icon.hash.Load().(string) != dep.hash {
			return false
		}
	}
	return true
}

func (s *referenceStore) completedList(ctx context.Context, key listResponseKey) (*completedListResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Counts are generation numbers: these indexes only gain committed streams
	// during a reference store's lifetime. Unrelated reservations do not invalidate
	// a user's or a tag's response. Snapshot under the same lock on a cache miss.
	s.indexMu.RLock()
	rows := s.allStreams
	if key.kind == 1 {
		rows = s.byTag[key.id]
	}
	if key.kind == 2 {
		rows = s.byUser[key.id]
	}
	key.count = len(rows)
	if key.kind == 0 && key.limit >= 0 && key.limit < len(rows) {
		rows = rows[:key.limit]
	}
	key.limit = len(rows)
	key.tags = currentTags.Load()
	shard := (uint64(key.id)*31 + uint64(key.kind)*7 + uint64(key.limit)) & 15
	cache := &s.listJSON[shard]
	if cached, ok := cache.getValid(key, (*completedListResponse).valid); ok {
		s.indexMu.RUnlock()
		return cached, nil
	}
	models := append([]*LivestreamModel(nil), rows...)
	s.indexMu.RUnlock()
	refs := newResponseJSONBuilder(ctx)
	refs.refs, refs.tags = s, key.tags
	data := []byte{'['}
	for i, model := range models {
		stream, err := refs.stream(model.ID)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			data = append(data, ',')
		}
		data = append(data, stream...)
	}
	data = append(data, ']')
	if key.pretty {
		var indented bytes.Buffer
		if err := json.Indent(&indented, data, "", "  "); err != nil {
			return nil, err
		}
		data = indented.Bytes()
	}
	data = append(data, '\n')
	result := &completedListResponse{plain: data, icons: make([]listIconDependency, 0, len(refs.users))}
	for id, user := range refs.users {
		value, _ := s.users.Load(id)
		result.icons = append(result.icons, listIconDependency{icon: value.(*userReference).icon, hash: user.hash})
	}
	// Search responses negotiate gzip as before. Store both encodings so a hit
	// only validates icon hashes and writes the already completed body.
	if key.kind != 2 && len(data) >= 1024 {
		compressed, err := compressResponseJSON(data)
		if err != nil {
			return nil, err
		}
		result.gzip = compressed
	}
	result.cost = 160 + cap(result.plain) + cap(result.gzip) + cap(result.icons)*48
	for _, dep := range result.icons {
		result.cost += len(dep.hash)
	}
	cache.replace(key, result, result.cost, listResponseBudget, func(r *completedListResponse) int { return r.cost })
	return result, nil
}

func (s *referenceStore) writeCompletedList(c echo.Context, key listResponseKey) error {
	_, key.pretty = c.QueryParams()["pretty"]
	key.pretty = key.pretty || c.Echo().Debug
	result, err := s.completedList(c.Request().Context(), key)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to fill livestream: "+err.Error())
	}
	data := result.plain
	if key.kind != 2 && c.Request().Method == http.MethodGet {
		c.Response().Header().Add("Vary", "Accept-Encoding")
		if len(result.gzip) > 0 && acceptsResponseGzip(c.Request().Header.Values("Accept-Encoding")) {
			c.Response().Header().Set("Content-Encoding", "gzip")
			c.Response().Header().Del("Content-Length")
			data = result.gzip
		}
	}
	return c.JSONBlob(http.StatusOK, data)
}
