package main

import (
	"crypto/sha256"
	"net/http"

	"github.com/labstack/echo/v4"
)

const timelineGzipBudget = 2 << 20 // Per shard; 32 MiB across all shards.

// Content-defined boundaries survive prepends and moderation without tracking
// an extra row sequence. Builders keep responses of up to 64 rows together and
// use groups of at least 32 rows for larger responses to preserve compression.
// A 128-row cap may absorb the final tail of fewer than 32 rows.
func timelineGroupBoundary(id int64) bool {
	x := uint64(id)
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x&31 == 0
}

func (s *referenceStore) timelineGzipMember(data []byte) ([]byte, error) {
	// The exact encoded bytes include punctuation, all row fields and mutable
	// icon/tag snapshots. No stale group can survive a dependency change.
	key := sha256.Sum256(data)
	cache := &s.timelineGzip[key[0]&15]
	if member, ok := cache.get(key); ok {
		s.timelineGzipHitBytes.Add(uint64(len(data)))
		return member, nil
	}
	s.timelineGzipMissBytes.Add(uint64(len(data)))
	member, err := compressResponseMember(data)
	if err != nil {
		return nil, err
	}
	cache.put(key, member, 128+cap(member), timelineGzipBudget)
	return member, nil
}

func writeTimelineGzip(c echo.Context, b *responseJSONBuffer) error {
	if c.Request().Method != http.MethodGet {
		return c.JSONBlob(http.StatusOK, b.data)
	}
	c.Response().Header().Add("Vary", "Accept-Encoding")
	if len(b.data) < 1024 || !acceptsResponseGzip(c.Request().Header.Values("Accept-Encoding")) {
		return c.JSONBlob(http.StatusOK, b.data)
	}
	out := acquireResponseJSON()
	defer releaseResponseJSON(out)
	out.data = out.data[:0]
	start := 0
	for _, end := range b.groupEnds {
		// Keep the closing bracket/newline in the final member instead of
		// emitting a separate gzip member containing only punctuation.
		if end == len(b.data)-2 {
			break
		}
		member, err := b.gzipStore.timelineGzipMember(b.data[start:end])
		if err != nil {
			return err
		}
		out.data = append(out.data, member...)
		start = end
	}
	member, err := b.gzipStore.timelineGzipMember(b.data[start:])
	if err != nil {
		return err
	}
	out.data = append(out.data, member...)
	// A gzip representation is a concatenation of members. Standard gzip
	// readers, including net/http's transparent decoder, decode every member.
	if len(out.data) >= len(b.data) {
		return c.JSONBlob(http.StatusOK, b.data)
	}
	c.Response().Header().Set("Content-Encoding", "gzip")
	c.Response().Header().Del("Content-Length")
	return c.JSONBlob(http.StatusOK, out.data)
}
