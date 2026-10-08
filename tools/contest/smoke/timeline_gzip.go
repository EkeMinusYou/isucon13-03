package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func checkTimelineGzip(client *http.Client) error {
	if _, err := checkJSONGzip(client, "/api/livestream/search?limit=10"); err != nil {
		return err
	}
	data, err := request(client, http.MethodGet, "/api/livestream/search?limit=10", "")
	if err != nil {
		return err
	}
	var streams []struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(data, &streams); err != nil || len(streams) == 0 {
		return fmt.Errorf("timeline gzip: no initialized livestream: %v", err)
	}
	for _, route := range []string{"livecomment", "reaction"} {
		for _, stream := range streams {
			path := fmt.Sprintf("/api/livestream/%d/%s?limit=12", stream.ID, route)
			compressed, err := checkJSONGzip(client, path)
			if err != nil {
				return err
			}
			if compressed {
				break
			}
		}
	}
	return nil
}

func checkJSONGzip(client *http.Client, path string) (bool, error) {
	plainRes, plain, err := requestResponse(client, http.MethodGet, path, "", map[string]string{"Accept-Encoding": "identity"})
	if err != nil || plainRes.StatusCode != http.StatusOK || !json.Valid(plain) || plainRes.Header.Get("Content-Encoding") != "" {
		return false, fmt.Errorf("%s: identity response failed: %v", path, err)
	}
	res, encoded, err := requestResponse(client, http.MethodGet, path, "", map[string]string{"Accept-Encoding": "gzip"})
	if err != nil || res.StatusCode != http.StatusOK || !strings.Contains(strings.Join(res.Header.Values("Vary"), ","), "Accept-Encoding") {
		return false, fmt.Errorf("%s: gzip negotiation failed: %v", path, err)
	}
	if res.Header.Get("Content-Encoding") == "gzip" {
		z, err := gzip.NewReader(bytes.NewReader(encoded))
		if err != nil {
			return false, err
		}
		decoded, err := io.ReadAll(io.LimitReader(z, 4<<20))
		z.Close()
		if err != nil || !bytes.Equal(decoded, plain) {
			return false, fmt.Errorf("%s: gzip changed the JSON response: %v", path, err)
		}
		fmt.Printf("%s body %d -> %d bytes\n", path, len(plain), len(encoded))
		return true, nil
	}
	if len(plain) >= 1024 || !bytes.Equal(encoded, plain) {
		return false, fmt.Errorf("%s: expected gzip for a large JSON response", path)
	}
	return false, nil
}
