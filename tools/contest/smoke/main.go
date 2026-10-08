// Command smoke verifies the authenticated API, access logs, and profiles.
// It runs on ENTRY_HOST and verifies each configured nginx ingress.
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
	iconEdge := flag.Bool("icon-edge", false, "verify nginx direct 304 and its access log identity")
	crossIngress := flag.Bool("cross-ingress", false, "verify DNS routing, shared sessions and icon updates across ingress hosts")
	registrationPhase := flag.String("registration-phase", "", "write or read to check durable user and DNS registration")
	referencesPhase := flag.String("references-phase", "", "write or read to check reference publication and reload")
	mutationsPhase := flag.String("mutations-phase", "", "write or read to check durable WAL events and statistics")
	reservationsPhase := flag.String("reservations-phase", "", "write or read to check durable WAL reservations")
	flag.StringVar(&compressionSamplesDirectory, "compression-samples-dir", "", "save decoded response JSON for compression comparisons")
	dnsServer := flag.String("dns-server", "", "authoritative DNS server address")
	dnsAddress := flag.String("dns-address", "", "expected public HTTP ingress address")
	dnsCommonAddress := flag.String("dns-common-address", "", "expected common-domain ingress address")
	flag.Parse()
	if *reservationsPhase != "" && *reservationsPhase != "write" && *reservationsPhase != "read" {
		log.Fatal("reservations-phase must be write or read")
	}
	if *mutationsPhase != "" && *mutationsPhase != "write" && *mutationsPhase != "read" {
		log.Fatal("mutations-phase must be write or read")
	}
	if *iconsPhase != "" && *iconsPhase != "write" && *iconsPhase != "read" {
		log.Fatal("icons-phase must be write or read")
	}
	if *registrationPhase != "" && *registrationPhase != "write" && *registrationPhase != "read" {
		log.Fatal("registration-phase must be write or read")
	}
	if *referencesPhase != "" && (*referencesPhase != "write" && *referencesPhase != "read" || *registrationPhase == "") {
		log.Fatal("references-phase requires write or read and registration-phase")
	}
	if *registrationPhase != "" && (*dnsServer == "" || net.ParseIP(*dnsAddress) == nil) {
		log.Fatal("registration checks require dns-server and dns-address")
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
	for ingress, addr := range strings.Split(*targets, ",") {
		client := ingressClient(addr, roots, nil)
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
		if err := checkTimelineGzip(client); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s: search/timeline gzip negotiation and decoded JSON passed\n", addr)
		if *reservationsPhase != "" {
			phase := *reservationsPhase
			if ingress > 0 {
				phase = "read"
			}
			if err := checkWALReservation(client, phase == "write"); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("%s: reservation %s, metadata, duplicate tags and references passed\n", addr, phase)
		}
		if *mutationsPhase != "" {
			phase := *mutationsPhase
			// Moderation changes shared state; later ingresses verify the first write.
			if ingress > 0 && phase == "write" {
				phase = "read"
			}
			if err := checkMutationPersistence(client, phase == "write"); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("%s: mutation %s, all WAL events, moderation, report snapshots and statistics persistence passed\n", addr, phase)
		}
		if *iconsPhase != "" {
			if err := checkIcons(client, addr, *iconsPhase == "write"); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("%s: icon %s, content/hash/ETag, conditional GET and internal access checks passed\n", addr, *iconsPhase)
		}
		if *iconEdge {
			if err := checkIconEdge(client); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("%s: nginx direct 304, current image and logged session identity passed\n", addr)
		}
		if *registrationPhase != "" {
			phase := *registrationPhase
			if ingress > 0 && phase == "write" {
				phase = "read"
			}
			if err := checkRegistration(client, *dnsServer, *dnsAddress, phase == "write"); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("%s: user/DNS registration %s, login and subdomain HTTPS passed\n", addr, phase)
		}
		if *referencesPhase != "" {
			if err := checkReferenceModels(client, *referencesPhase == "write"); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("%s: reference %s, reservation, duplicate tags and nested comment metadata passed\n", addr, *referencesPhase)
		}
		client.CloseIdleConnections()
	}
	if *crossIngress {
		if err := checkCrossIngress(strings.Split(*targets, ","), roots, *dnsServer, *dnsAddress, *dnsCommonAddress); err != nil {
			log.Fatal(err)
		}
		fmt.Println("DNS routing, shared session and bidirectional icon updates within two seconds passed")
	}
}

func ingressClient(addr string, roots *x509.CertPool, jar http.CookieJar) *http.Client {
	if jar == nil {
		jar, _ = cookiejar.New(nil)
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, addr)
		},
	}
	return &http.Client{Transport: transport, Jar: jar, Timeout: 10 * time.Second}
}

type smokeReferenceUser struct {
	Name     string `json:"name"`
	IconHash string `json:"icon_hash"`
	Theme    struct {
		DarkMode bool `json:"dark_mode"`
	} `json:"theme"`
}

type smokeReferenceStream struct {
	ID    int64              `json:"id"`
	Title string             `json:"title"`
	Owner smokeReferenceUser `json:"owner"`
	Tags  []struct {
		ID int64 `json:"id"`
	} `json:"tags"`
}

func checkReferenceModels(client *http.Client, write bool) error {
	const title = "Reference model smoke"
	if write {
		data, err := request(client, http.MethodGet, "/api/tag", "")
		if err != nil {
			return err
		}
		var catalog struct {
			Tags []struct {
				ID int64 `json:"id"`
			} `json:"tags"`
		}
		if err := json.Unmarshal(data, &catalog); err != nil || len(catalog.Tags) < 2 {
			return fmt.Errorf("reference tags: invalid catalog: %v", err)
		}
		body, _ := json.Marshal(struct {
			Title   string  `json:"title"`
			Tags    []int64 `json:"tags"`
			StartAt int64   `json:"start_at"`
			EndAt   int64   `json:"end_at"`
		}{title, []int64{catalog.Tags[1].ID, catalog.Tags[0].ID, catalog.Tags[1].ID}, 1700874000, 1700877600})
		res, _, err := requestResponse(client, http.MethodPost, "/api/livestream/reservation", string(body), nil)
		if err != nil || res.StatusCode != http.StatusCreated {
			return fmt.Errorf("reference reservation failed: %v", err)
		}
	}
	data, err := request(client, http.MethodGet, "/api/user/role-split-smoke/livestream", "")
	if err != nil {
		return err
	}
	var streams []smokeReferenceStream
	if err := json.Unmarshal(data, &streams); err != nil {
		return err
	}
	var stream smokeReferenceStream
	for _, entry := range streams {
		if entry.Title == title {
			stream = entry
		}
	}
	if stream.ID == 0 || stream.Owner.Name != "role-split-smoke" || !stream.Owner.Theme.DarkMode || len(stream.Owner.IconHash) != 64 || len(stream.Tags) != 3 || stream.Tags[0].ID > stream.Tags[1].ID || stream.Tags[1].ID != stream.Tags[2].ID {
		return fmt.Errorf("reference stream was not published/restored with ordered duplicate tags")
	}
	path := fmt.Sprintf("/api/livestream/%d/livecomment", stream.ID)
	if write {
		res, _, err := requestResponse(client, http.MethodPost, path, `{"comment":"Reference smoke comment","tip":123}`, nil)
		if err != nil || res.StatusCode != http.StatusCreated {
			return fmt.Errorf("reference comment failed: %v", err)
		}
	}
	data, err = request(client, http.MethodGet, path, "")
	if err != nil {
		return err
	}
	var comments []struct {
		Comment    string               `json:"comment"`
		Tip        int64                `json:"tip"`
		User       smokeReferenceUser   `json:"user"`
		Livestream smokeReferenceStream `json:"livestream"`
	}
	if err := json.Unmarshal(data, &comments); err != nil || len(comments) != 1 || comments[0].Comment != "Reference smoke comment" || comments[0].Tip != 123 || comments[0].User != stream.Owner {
		return fmt.Errorf("reference comment metadata differs from committed user: %v", err)
	}
	want, _ := json.Marshal(stream)
	got, _ := json.Marshal(comments[0].Livestream)
	if !bytes.Equal(want, got) {
		return fmt.Errorf("reference comment has stale livestream metadata")
	}
	return nil
}

func checkRegistration(client *http.Client, dnsServer, dnsAddress string, write bool) error {
	const username = "role-split-smoke"
	if write {
		res, _, err := requestResponse(client, http.MethodPost, "/api/register", `{"name":"role-split-smoke","display_name":"Topology smoke","description":"Topology smoke","password":"topology-smoke-password","theme":{"dark_mode":true}}`, nil)
		if err != nil {
			return err
		}
		if res.StatusCode != http.StatusCreated {
			return fmt.Errorf("registration: status %d", res.StatusCode)
		}
	}
	if _, err := request(client, http.MethodPost, "/api/login", `{"username":"role-split-smoke","password":"topology-smoke-password"}`); err != nil {
		return err
	}
	data, err := request(client, http.MethodGet, "/api/user/me", "")
	if err != nil {
		return err
	}
	var user struct {
		Name     string `json:"name"`
		IconHash string `json:"icon_hash"`
		Theme    struct {
			DarkMode bool `json:"dark_mode"`
		} `json:"theme"`
	}
	if err := json.Unmarshal(data, &user); err != nil || user.Name != username || !user.Theme.DarkMode || len(user.IconHash) != 64 {
		return fmt.Errorf("registered user reference was not published or restored: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "udp", dnsServer)
	}}
	addresses, err := resolver.LookupIP(ctx, "ip4", username+".u.isucon.dev")
	if err != nil {
		return fmt.Errorf("registered user DNS: %w", err)
	}
	if len(addresses) != 1 || !addresses[0].Equal(net.ParseIP(dnsAddress)) {
		return fmt.Errorf("registered user DNS: expected %s, got %v", dnsAddress, addresses)
	}
	req, err := http.NewRequest(http.MethodGet, "https://"+username+".u.isucon.dev/api/user/"+username+"/theme", nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("registered user subdomain HTTPS: %w", err)
	}
	defer res.Body.Close()
	var theme struct {
		DarkMode bool `json:"dark_mode"`
	}
	if err := json.NewDecoder(res.Body).Decode(&theme); err != nil || res.StatusCode != http.StatusOK || !theme.DarkMode {
		return fmt.Errorf("registered user subdomain HTTPS: status %d, invalid theme: %v", res.StatusCode, err)
	}
	return nil
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
	// Both ingress hosts share the same durable user and icon owner.
	image := append(fallback, []byte("\nicon-smoke:shared-owner")...)
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
