package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const dnsZone = "u.isucon.dev"

var dnsSerialLine = regexp.MustCompile(`(?m)^\s*[0-9]+\s*; serial`)
var dnsGeneration atomic.Uint64
var dnsHTTP = &http.Client{Timeout: 8 * time.Second}

type dnsUpdate struct {
	Name     string `json:"name,omitempty"`
	Reset    bool   `json:"reset,omitempty"`
	ReadOnly bool   `json:"-"`
	Epoch    uint64 `json:"epoch"`
	done     chan dnsReply
}
type dnsReply struct {
	Epoch  uint64 `json:"epoch"`
	Serial uint32 `json:"serial"`
	Error  string `json:"error,omitempty"`
}
type dnsControl struct {
	base      string
	zonePath  string
	names     map[string]bool
	epoch     uint64
	serial    uint32
	published bool
	dirty     bool
	queue     chan dnsUpdate
	reload    func(uint32) error
}

func validDNSUser(name string) bool {
	if len(name) == 0 || len(name)+len(dnsZone)+1 > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
	}
	return true
}

func dnsRecordName(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, `\%03d`, c)
		}
	}
	return b.String()
}

func newDNSControl(base, zonePath string, reload func(uint32) error) (*dnsControl, error) {
	d := &dnsControl{base: base, zonePath: zonePath, names: make(map[string]bool), epoch: 1, serial: uint32(time.Now().Unix()), queue: make(chan dnsUpdate, 128), reload: reload}
	data, err := os.ReadFile(zonePath)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		dynamic := false
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if strings.HasPrefix(line, "; ISUPIPE EPOCH ") {
				d.epoch, _ = strconv.ParseUint(strings.TrimPrefix(line, "; ISUPIPE EPOCH "), 10, 64)
				dynamic = true
				continue
			}
			if len(fields) >= 3 && fields[1] == ";" && fields[2] == "serial" {
				n, _ := strconv.ParseUint(fields[0], 10, 32)
				d.serial = uint32(n)
			}
			if dynamic && len(fields) == 5 && fields[1] == "0" && fields[2] == "IN" && fields[3] == "A" {
				d.names[fields[0]] = true
			}
		}
	}
	text, err := d.zoneContent()
	if err != nil {
		return nil, err
	}
	// Readdress existing names without resetting the registration generation.
	if !bytes.Equal(data, text) {
		if data != nil {
			d.serial++
		}
		if err := d.persist(); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func (d *dnsControl) zoneContent() ([]byte, error) {
	if !dnsSerialLine.MatchString(d.base) {
		return nil, fmt.Errorf("base zone has no SOA serial")
	}
	text := dnsSerialLine.ReplaceAllString(d.base, fmt.Sprintf("    %d ; serial", d.serial))
	names := make([]string, 0, len(d.names))
	for name := range d.names {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(text)
	fmt.Fprintf(&b, "\n; ISUPIPE EPOCH %d\n", d.epoch)
	address := os.Getenv(powerDNSSubdomainAddressEnvKey)
	for _, name := range names {
		fmt.Fprintf(&b, "%s 0 IN A %s\n", name, address)
	}
	return []byte(b.String()), nil
}

func (d *dnsControl) persist() error {
	data, err := d.zoneContent()
	if err != nil {
		return err
	}
	dir := filepath.Dir(d.zonePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".zone-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0644); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), d.zonePath); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (d *dnsControl) run() {
	var pending *dnsUpdate
	for {
		var first dnsUpdate
		if pending != nil {
			first = *pending
			pending = nil
		} else {
			r, ok := <-d.queue
			if !ok {
				return
			}
			first = r
		}
		batch := []dnsUpdate{first}
		timer := time.NewTimer(2 * time.Millisecond)
	collect:
		for len(batch) < 32 && !first.Reset {
			select {
			case r, ok := <-d.queue:
				if !ok {
					break collect
				}
				if r.Reset {
					pending = &r
					break collect
				}
				batch = append(batch, r)
			case <-timer.C:
				break collect
			}
		}
		timer.Stop()
		results := make([]dnsReply, len(batch))
		changed := false
		for i, r := range batch {
			if r.ReadOnly {
				results[i].Epoch = d.epoch
				continue
			}
			if r.Reset {
				d.names = make(map[string]bool)
				d.epoch++
				changed = true
			} else {
				if r.Epoch != d.epoch {
					results[i].Error = "stale DNS generation"
					continue
				}
				name := dnsRecordName(r.Name)
				if !d.names[name] {
					d.names[name] = true
					changed = true
				}
			}
			results[i].Epoch = d.epoch
		}
		var err error
		if changed {
			d.serial++
			d.published = false
			d.dirty = true
		}
		if d.dirty {
			err = d.persist()
			if err == nil {
				d.dirty = false
			}
		}
		// A failed publish is retried even when the requested name already exists.
		if err == nil && !d.published {
			err = d.reload(d.serial)
			if err == nil {
				d.published = true
			}
		}
		for i, r := range batch {
			results[i].Serial = d.serial
			if err != nil && results[i].Error == "" {
				results[i].Error = err.Error()
			}
			r.done <- results[i]
		}
	}
}

func (d *dnsControl) handler(allowed map[string]bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if !allowed[host] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var update dnsUpdate
		if r.URL.Path == "/register" {
			if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&update); err != nil || !validDNSUser(update.Name) {
				http.Error(w, "invalid DNS name", 400)
				return
			}
			update.Reset = false
		} else if r.URL.Path == "/reset" {
			update.Reset = true
		} else if r.URL.Path == "/generation" {
			update.ReadOnly = true
		} else {
			http.NotFound(w, r)
			return
		}
		update.done = make(chan dnsReply, 1)
		select {
		case d.queue <- update:
		case <-r.Context().Done():
			return
		}
		select {
		case reply := <-update.done:
			w.Header().Set("Content-Type", "application/json")
			if reply.Error != "" {
				w.WriteHeader(http.StatusInternalServerError)
			}
			_ = json.NewEncoder(w).Encode(reply)
		case <-r.Context().Done():
		}
	})
	return mux
}

func reloadDNSZone(serial uint32) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pdns_control", "bind-reload-now", dnsZone).CombinedOutput()
	if err != nil {
		return fmt.Errorf("reload DNS zone: %w: %s", err, out)
	}
	query, err := (&dnsmessage.Message{Header: dnsmessage.Header{ID: 1234}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(dnsZone + "."), Type: dnsmessage.TypeSOA, Class: dnsmessage.ClassINET}}}).Pack()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("udp", "127.0.0.1:53", 100*time.Millisecond)
		if err != nil {
			return err
		}
		_ = conn.SetDeadline(time.Now().Add(100 * time.Millisecond))
		_, err = conn.Write(query)
		buf := make([]byte, 4096)
		var n int
		if err == nil {
			n, err = conn.Read(buf)
		}
		conn.Close()
		if err == nil {
			var msg dnsmessage.Message
			if msg.Unpack(buf[:n]) == nil {
				for _, answer := range msg.Answers {
					if soa, ok := answer.Body.(*dnsmessage.SOAResource); ok && soa.Serial == serial {
						return nil
					}
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("DNS did not publish SOA serial %d: %s", serial, out)
}

func runDNSControl() error {
	base, err := os.ReadFile("/etc/powerdns/u.isucon.dev.zone")
	if err != nil {
		return err
	}
	address := os.Getenv(powerDNSSubdomainAddressEnvKey)
	if net.ParseIP(address) == nil {
		return fmt.Errorf("invalid DNS record address")
	}
	base = []byte(strings.ReplaceAll(string(base), "<ISUCON_SUBDOMAIN_ADDRESS>", address))
	d, err := newDNSControl(string(base), "/var/lib/isupipe/dns/u.isucon.dev.zone", reloadDNSZone)
	if err != nil {
		return err
	}
	allowed := map[string]bool{"127.0.0.1": true}
	for _, ip := range strings.Fields(os.Getenv("ISUPIPE_DNS_CLIENTS")) {
		allowed[ip] = true
	}
	listener, err := net.Listen("tcp", os.Getenv("ISUPIPE_DNS_LISTEN"))
	if err != nil {
		return err
	}
	go d.run()
	if socket := os.Getenv("NOTIFY_SOCKET"); socket != "" {
		conn, err := net.Dial("unixgram", socket)
		if err != nil {
			return err
		}
		_, err = conn.Write([]byte("READY=1"))
		conn.Close()
		if err != nil {
			return err
		}
	}
	return (&http.Server{Handler: d.handler(allowed), ReadHeaderTimeout: time.Second}).Serve(listener)
}

func updateDNS(ctx context.Context, path, name string) error {
	body, err := json.Marshal(dnsUpdate{Name: name, Epoch: dnsGeneration.Load()})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, os.Getenv("ISUPIPE_DNS_URL")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := dnsHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var reply dnsReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK || reply.Error != "" {
		return fmt.Errorf("DNS update: status=%d %s", resp.StatusCode, reply.Error)
	}
	if path == "/reset" || path == "/generation" {
		dnsGeneration.Store(reply.Epoch)
	}
	return nil
}
