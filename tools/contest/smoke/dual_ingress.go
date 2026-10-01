package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"time"
)

func checkCrossIngress(addresses []string, roots *x509.CertPool, dnsServer, streamAddress, commonAddress string) error {
	if len(addresses) < 2 || dnsServer == "" || net.ParseIP(streamAddress) == nil || net.ParseIP(commonAddress) == nil {
		return fmt.Errorf("cross-ingress checks require two ingress hosts and both DNS addresses")
	}
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "udp", dnsServer)
	}}
	for _, route := range []struct{ name, address string }{
		{"pipe.u.isucon.dev", commonAddress},
		{"test001.u.isucon.dev", streamAddress},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ips, err := resolver.LookupIP(ctx, "ip4", route.name)
		cancel()
		if err != nil || len(ips) != 1 || !ips[0].Equal(net.ParseIP(route.address)) {
			return fmt.Errorf("DNS %s: expected %s, got %v: %v", route.name, route.address, ips, err)
		}
	}
	jar, _ := cookiejar.New(nil)
	clients := make([]*http.Client, len(addresses))
	for i, addr := range addresses {
		clients[i] = ingressClient(addr, roots, jar)
		clients[i].Timeout = time.Second
		defer clients[i].CloseIdleConnections()
	}
	if _, err := request(clients[0], http.MethodPost, "/api/login", `{"username":"test001","password":"test"}`); err != nil {
		return err
	}
	for i, client := range clients {
		if _, err := request(client, http.MethodGet, "/api/user/me", ""); err != nil {
			return fmt.Errorf("shared session on %s: %w", addresses[i], err)
		}
	}
	fallback, err := os.ReadFile("/home/isucon/webapp/img/NoImage.jpg")
	if err != nil {
		return err
	}
	// Write through each ingress, then check the changed validator on every host.
	for writer, client := range clients {
		data, err := request(client, http.MethodGet, "/api/user/test001", "")
		if err != nil {
			return err
		}
		var before struct {
			IconHash string `json:"icon_hash"`
		}
		if err := json.Unmarshal(data, &before); err != nil {
			return err
		}
		oldETag := "\"" + before.IconHash + "\""
		for _, reader := range clients {
			if _, _, err := requestResponse(reader, http.MethodGet, "/api/user/test001/icon", "", map[string]string{"If-None-Match": oldETag}); err != nil {
				return err
			}
		}
		image := append(append([]byte{}, fallback...), []byte(fmt.Sprintf("\ncross-ingress:%d:%d", writer, time.Now().UnixNano()))...)
		hash := fmt.Sprintf("%x", sha256.Sum256(image))
		body, _ := json.Marshal(struct {
			Image []byte `json:"image"`
		}{image})
		deadline := time.Now().Add(2 * time.Second)
		res, _, err := requestResponse(client, http.MethodPost, "/api/icon", string(body), nil)
		if err != nil || res.StatusCode != http.StatusCreated {
			return fmt.Errorf("cross-ingress upload on %s: %v", addresses[writer], err)
		}
		for i, reader := range clients {
			updated := false
			for time.Now().Before(deadline) {
				res, data, err := requestResponse(reader, http.MethodGet, "/api/user/test001/icon", "", map[string]string{"If-None-Match": oldETag})
				if err != nil {
					return err
				}
				if res.StatusCode == http.StatusOK && bytes.Equal(data, image) && res.Header.Get("ETag") == "\""+hash+"\"" {
					updated = true
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if !updated || time.Now().After(deadline) {
				return fmt.Errorf("icon update from %s did not reach %s within two seconds", addresses[writer], addresses[i])
			}
			data, err := request(reader, http.MethodGet, "/api/user/test001", "")
			if err != nil {
				return err
			}
			var user struct {
				IconHash string `json:"icon_hash"`
			}
			if err := json.Unmarshal(data, &user); err != nil || user.IconHash != hash {
				return fmt.Errorf("cross-ingress API hash differs on %s", addresses[i])
			}
			res, data, err = requestResponse(reader, http.MethodGet, "/api/user/test001/icon", "", map[string]string{"If-None-Match": "\"" + hash + "\""})
			if err != nil || res.StatusCode != http.StatusNotModified || len(data) != 0 {
				return fmt.Errorf("cross-ingress updated ETag failed on %s: %v", addresses[i], err)
			}
		}
	}
	return nil
}
