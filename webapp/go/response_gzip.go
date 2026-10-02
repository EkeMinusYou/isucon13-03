package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"
)

type responseGzip struct {
	buffer bytes.Buffer
	writer *gzip.Writer
}

var responseGzipPool = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return &responseGzip{writer: w}
}}

// An explicit gzip exclusion overrides a wildcard. Invalid quality values
// never opt a client into compression.
func acceptsResponseGzip(headers []string) bool {
	explicit, wildcard := false, false
	seen := false
	for _, header := range headers {
		for _, item := range strings.Split(header, ",") {
			parts := strings.Split(item, ";")
			coding := strings.TrimSpace(parts[0])
			if !strings.EqualFold(coding, "gzip") && coding != "*" {
				continue
			}
			quality := 1.0
			qualitySeen := false
			for _, param := range parts[1:] {
				key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") || qualitySeen {
					quality = 0
					break
				}
				qualitySeen = true
				parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
				if err != nil || math.IsNaN(parsed) || parsed < 0 || parsed > 1 {
					quality = 0
					break
				}
				quality = parsed
			}
			if strings.EqualFold(coding, "gzip") {
				// Preserve exclusions even if a duplicate token allows gzip.
				if !seen {
					explicit = quality > 0
				} else {
					explicit = explicit && quality > 0
				}
				seen = true
			} else {
				wildcard = wildcard || quality > 0
			}
		}
	}
	if seen {
		return explicit
	}
	return wildcard
}

func writeJSONEncoding(c echo.Context, data []byte, compress bool) error {
	if !compress || c.Request().Method != http.MethodGet {
		return c.JSONBlob(http.StatusOK, data)
	}
	c.Response().Header().Add("Vary", "Accept-Encoding")
	if len(data) < 1024 || !acceptsResponseGzip(c.Request().Header.Values("Accept-Encoding")) {
		return c.JSONBlob(http.StatusOK, data)
	}
	state := responseGzipPool.Get().(*responseGzip)
	state.buffer.Reset()
	state.writer.Reset(&state.buffer)
	defer func() {
		if state.buffer.Cap() <= 128<<10 {
			responseGzipPool.Put(state)
		}
	}()
	if _, err := state.writer.Write(data); err != nil {
		return err
	}
	if err := state.writer.Close(); err != nil {
		return err
	}
	if state.buffer.Len() >= len(data) {
		return c.JSONBlob(http.StatusOK, data)
	}
	c.Response().Header().Set("Content-Encoding", "gzip")
	c.Response().Header().Del("Content-Length")
	return c.JSONBlob(http.StatusOK, state.buffer.Bytes())
}
