package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDNSUpdatesPersistPublishAndReset(t *testing.T) {
	t.Setenv(powerDNSSubdomainAddressEnvKey, "127.0.0.1")
	zone := filepath.Join(t.TempDir(), "zone")
	base := "$TTL 0\n@ SOA ns1 hostmaster (\n    0 ; serial\n1 1 1 1)\npipe 0 IN A 127.0.0.1\n"
	fail := true
	d, err := newDNSControl(base, zone, func(serial uint32) error {
		if fail {
			fail = false
			return fmt.Errorf("publish failed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	go d.run()
	defer close(d.queue)
	server := httptest.NewServer(d.handler(map[string]bool{"127.0.0.1": true}))
	defer server.Close()
	call := func(path string, name string, epoch uint64) (dnsReply, int) {
		t.Helper()
		body, _ := json.Marshal(dnsUpdate{Name: name, Epoch: epoch})
		response, err := http.Post(server.URL+path, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var reply dnsReply
		if err := json.NewDecoder(response.Body).Decode(&reply); err != nil {
			t.Fatal(err)
		}
		return reply, response.StatusCode
	}
	first, status := call("/register", "new-user", 1)
	if status != 500 {
		t.Fatalf("failed publish acknowledged: %+v", first)
	}
	retry, status := call("/register", "new-user", 1)
	if status != 200 || retry.Serial != first.Serial {
		t.Fatalf("retry must publish durable generation: %+v status=%d", retry, status)
	}
	data, err := os.ReadFile(zone)
	if err != nil || !strings.Contains(string(data), "new-user 0 IN A 127.0.0.1") {
		t.Fatalf("durable record: %s %v", data, err)
	}
	restored, err := newDNSControl(base, zone, func(uint32) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !restored.names["new-user"] || restored.epoch != retry.Epoch || restored.serial != retry.Serial {
		t.Fatal("restart lost DNS state")
	}
	reset, status := call("/reset", "", 0)
	if status != 200 || reset.Epoch <= retry.Epoch {
		t.Fatalf("reset generation: %+v", reset)
	}
	_, status = call("/register", "late-user", retry.Epoch)
	if status != 500 {
		t.Fatal("stale generation accepted")
	}
	data, _ = os.ReadFile(zone)
	if strings.Contains(string(data), "new-user 0 IN A") || strings.Contains(string(data), "late-user 0 IN A") {
		t.Fatal("reset retained prior registration")
	}
}

func TestDNSNameValidation(t *testing.T) {
	for _, name := range []string{"test001", "a-b", "nested.name", "under_score", "日本語", "a b", "a\nb 0 IN A 1.2.3.4"} {
		if !validDNSUser(name) {
			t.Errorf("rejected %q", name)
		}
	}
	for _, name := range []string{"", "a..b", strings.Repeat("x", 64)} {
		if validDNSUser(name) {
			t.Errorf("accepted %q", name)
		}
	}
	if got := dnsRecordName("A\nb ; IN A 1.2.3.4"); got != `a\010b\032\059\032in\032a\0321.2.3.4` {
		t.Fatalf("unsafe zone owner: %q", got)
	}
}
