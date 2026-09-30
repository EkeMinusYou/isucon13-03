// Command smoke verifies the authenticated API, access logs, and profiles.
// It runs on ENTRY_HOST and contacts each existing independent nginx.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"time"
)

func main() {
	targets := flag.String("ingress", "127.0.0.1:443", "comma separated ingress addresses")
	iconsPhase := flag.String("icons-phase", "", "write or read to check durable icon updates")
	flag.Parse()
	if *iconsPhase != "" && *iconsPhase != "write" && *iconsPhase != "read" {
		log.Fatal("icons-phase must be write or read")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		log.Fatal(err)
	}
	cert, err := os.ReadFile("/etc/nginx/tls/_.u.isucon.dev.crt")
	if err != nil {
		log.Fatal(err)
	}
	if !roots.AppendCertsFromPEM(cert) {
		log.Fatal("cannot load the supplied public certificate")
	}
	// Use TLS verification and the official cookie domain while dialing the
	// concrete ingress host. No credentials or session values are printed.
	for _, addr := range strings.Split(*targets, ",") {
		jar, err := cookiejar.New(nil)
		if err != nil {
			log.Fatal(err)
		}
		transport := &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, addr)
			},
		}
		client := &http.Client{Transport: transport, Jar: jar, Timeout: 10 * time.Second}
		if _, err := request(client, http.MethodPost, "/api/login", `{"username":"test001","password":"test"}`); err != nil {
			log.Fatal(err)
		}
		paths := []string{"/api/user/me", "/api/tag", "/api/user/test001", "/api/user/test001/theme", "/api/livestream/search?limit=1", "/api/livestream", "/api/user/test001/statistics"}
		for _, path := range paths {
			data, err := request(client, http.MethodGet, path, "")
			if err != nil {
				log.Fatal(err)
			}
			if !json.Valid(data) {
				log.Fatalf("%s: invalid JSON", path)
			}
			if path == "/api/livestream/search?limit=1" {
				var livestreams []struct {
					ID int64 `json:"id"`
				}
				if err := json.Unmarshal(data, &livestreams); err != nil || len(livestreams) == 0 {
					log.Fatalf("%s: expected an initialized livestream", path)
				}
				stats, err := request(client, http.MethodGet, fmt.Sprintf("/api/livestream/%d/statistics", livestreams[0].ID), "")
				if err != nil {
					log.Fatal(err)
				}
				if !json.Valid(stats) {
					log.Fatal("livestream statistics: invalid JSON")
				}
			}
		}
		fmt.Printf("%s: login and eight authenticated reads passed\n", addr)
		if *iconsPhase != "" {
			if err := checkIcons(client, addr, *iconsPhase == "write"); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("%s: icon %s, content/hash/ETag, conditional GET and internal access checks passed\n", addr, *iconsPhase)
		}
		transport.CloseIdleConnections()
	}
}

func request(client *http.Client, method, path, body string) ([]byte, error) {
	res, data, err := requestResponse(client, method, path, body, nil)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: status %d", method, path, res.StatusCode)
	}
	return data, nil
}

func requestResponse(client *http.Client, method, path, body string, headers map[string]string) (*http.Response, []byte, error) {
	req, err := http.NewRequest(method, "https://pipe.u.isucon.dev"+path, strings.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, nil, err
	}
	return res, data, nil
}

func checkIcons(client *http.Client, addr string, write bool) error {
	fallback, err := os.ReadFile("/home/isucon/webapp/img/NoImage.jpg")
	if err != nil {
		return err
	}
	// Trailing JPEG bytes keep the supplied image decodable and identify each host.
	image := append(fallback, []byte("\nicon-smoke:"+addr)...)
	hash := fmt.Sprintf("%x", sha256.Sum256(image))
	if write {
		body, _ := json.Marshal(struct {
			Image []byte `json:"image"`
		}{image})
		res, data, err := requestResponse(client, http.MethodPost, "/api/icon", string(body), nil)
		if err != nil {
			return err
		}
		var result struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(data, &result); err != nil || res.StatusCode != http.StatusCreated || result.ID <= 0 {
			return fmt.Errorf("%s: icon upload status=%d id=%d err=%v", addr, res.StatusCode, result.ID, err)
		}
	}
	data, err := request(client, http.MethodGet, "/api/user/test001", "")
	if err != nil {
		return err
	}
	var user struct {
		IconHash string `json:"icon_hash"`
	}
	if err := json.Unmarshal(data, &user); err != nil || user.IconHash != hash {
		return fmt.Errorf("%s: user hash differs from uploaded image: err=%v", addr, err)
	}
	fallbackHash := fmt.Sprintf("%x", sha256.Sum256(fallback))
	for _, tc := range []struct {
		headers map[string]string
		status  int
	}{
		{nil, http.StatusOK},
		{map[string]string{"If-None-Match": "\"" + hash + "\""}, http.StatusNotModified},
		{map[string]string{"If-None-Match": "\"" + fallbackHash + "\""}, http.StatusOK},
		{map[string]string{"If-Modified-Since": "Sun, 01 Jan 2034 00:00:00 GMT"}, http.StatusOK},
	} {
		res, data, err := requestResponse(client, http.MethodGet, "/api/user/test001/icon", "", tc.headers)
		if err != nil {
			return err
		}
		if res.StatusCode != tc.status || res.Header.Get("ETag") != "\""+hash+"\"" || res.Header.Get("X-Accel-Redirect") != "" {
			return fmt.Errorf("%s: icon status=%d want=%d or incorrect public headers", addr, res.StatusCode, tc.status)
		}
		if tc.status == http.StatusOK && (!bytes.Equal(data, image) || res.Header.Get("Content-Type") != "image/jpeg") {
			return fmt.Errorf("%s: incorrect image body or content type", addr)
		}
		if tc.status == http.StatusNotModified && len(data) != 0 {
			return fmt.Errorf("%s: 304 has a body", addr)
		}
	}
	for _, path := range []string{"/api/user/missing-icon-smoke-user/icon", "/_internal/icons/" + hash} {
		res, _, err := requestResponse(client, http.MethodGet, path, "", nil)
		if err != nil {
			return err
		}
		if res.StatusCode != http.StatusNotFound {
			return fmt.Errorf("%s: %s status=%d want=404", addr, path, res.StatusCode)
		}
	}
	return nil
}
