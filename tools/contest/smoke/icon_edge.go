package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func checkIconEdge(client *http.Client) error {
	data, err := request(client, http.MethodGet, "/api/user/test001", "")
	if err != nil {
		return err
	}
	var user struct {
		IconHash string `json:"icon_hash"`
	}
	if err := json.Unmarshal(data, &user); err != nil {
		return err
	}
	res, image, err := requestResponse(client, http.MethodGet, "/api/user/test001/icon", "", nil)
	if err != nil {
		return err
	}
	if res.StatusCode != 200 || fmt.Sprintf("%x", sha256.Sum256(image)) != user.IconHash {
		return fmt.Errorf("icon edge: unconditional GET differs from current user hash")
	}
	u, _ := url.Parse("https://pipe.u.isucon.dev")
	var sessionID string
	for _, cookie := range client.Jar.Cookies(u) {
		if cookie.Name == "SESSIONID" {
			token, err := base64.RawURLEncoding.DecodeString(cookie.Value)
			if err != nil || len(token) != 65 {
				return fmt.Errorf("icon edge: invalid session format")
			}
			sessionID = hex.EncodeToString(token[33:])
		}
	}
	if sessionID == "" {
		return fmt.Errorf("icon edge: missing session identity")
	}
	file, err := os.Open("/var/log/nginx/access.log")
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	until := time.Now().Add(2 * time.Second)
	var captured []byte
	for time.Now().Before(until) {
		res, body, err := requestResponse(client, http.MethodGet, "/api/user/test001/icon", "", map[string]string{"If-None-Match": "\"" + user.IconHash + "\""})
		if err != nil {
			return err
		}
		if res.StatusCode != 304 || len(body) != 0 || res.Header.Get("ETag") != "\""+user.IconHash+"\"" || res.Header.Get("X-Measurement-Session-ID") != "" {
			return fmt.Errorf("icon edge: invalid conditional response")
		}
		chunk, err := io.ReadAll(file)
		if err != nil {
			return err
		}
		captured = append(captured, chunk...)
		for _, line := range strings.Split(string(captured), "\n") {
			var entry struct {
				URI       string `json:"uri"`
				Status    int    `json:"status"`
				Upstream  string `json:"upstream_addr"`
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal([]byte(line), &entry) == nil && entry.URI == "/api/user/test001/icon" && entry.Status == 304 && (entry.Upstream == "" || entry.Upstream == "-") && entry.SessionID == sessionID {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("icon edge: no direct 304 with matching session identity in access log")
}
