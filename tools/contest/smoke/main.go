// Command smoke verifies the authenticated API, access logs, and profiles.
// It runs on ENTRY_HOST and contacts each existing independent nginx.
package main

import (
	"context"
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
	flag.Parse()
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
		transport.CloseIdleConnections()
		fmt.Printf("%s: login and eight authenticated reads passed\n", addr)
	}
}

func request(client *http.Client, method, path, body string) ([]byte, error) {
	req, err := http.NewRequest(method, "https://pipe.u.isucon.dev"+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: status %d", method, path, res.StatusCode)
	}
	return data, nil
}
