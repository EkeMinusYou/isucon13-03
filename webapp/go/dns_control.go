package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const dnsZone = "u.isucon.dev"
const dnsSuffix = dnsZone + "."
const dnsSyncInterval = 100 * time.Millisecond
const dnsSyncBytes = 64 << 10

var dnsGeneration atomic.Uint64
var dnsHTTP = &http.Client{Timeout: 8 * time.Second}

type dnsUpdate struct {
	Name  string `json:"name,omitempty"`
	Reset bool   `json:"reset,omitempty"`
	Epoch uint64 `json:"epoch"`
	done  chan dnsReply
}
type dnsReply struct {
	Epoch  uint64 `json:"epoch"`
	Serial uint32 `json:"serial"`
	Error  string `json:"error,omitempty"`
	status int
}
type dnsState struct {
	names      map[string]bool
	epoch      uint64
	baseSerial uint32
	seq        uint64
	dynamic    int
	memory     int64
	ready      bool
	failed     bool
}

func (s *dnsState) serial() uint32 { return s.baseSerial + uint32(s.seq) }

type dnsCounters struct {
	updates    atomic.Uint64
	syncs      atomic.Uint64
	syncNanos  atomic.Uint64
	queries    atomic.Uint64
	answers    atomic.Uint64
	nxdomain   atomic.Uint64
	nodata     atomic.Uint64
	sendErrors atomic.Uint64
}
type dnsControl struct {
	mu        sync.RWMutex
	state     dnsState
	base      *dnsBase
	path      string
	file      *os.File
	ops       dnsFileOps
	queue     chan dnsUpdate
	life      sync.RWMutex
	closed    bool
	writer    sync.WaitGroup
	counters  dnsCounters
	syncMu    sync.Mutex
	syncWake  chan struct{}
	syncDone  chan struct{}
	syncedSeq uint64 // Protected by mu; reset and publication use the same generation.
	syncErr   error  // Protected by syncMu.
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
func dnsCanonical(name string) string {
	b := []byte(name)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
func dnsUserName(name string) (string, error) {
	if !validDNSUser(name) {
		return "", fmt.Errorf("invalid DNS name")
	}
	return dnsCanonical(name + "." + dnsSuffix), nil
}
func dnsAncestors(name string) []string {
	var names []string
	for {
		names = append(names, name)
		if name == dnsSuffix {
			return names
		}
		i := strings.IndexByte(name, '.')
		if i < 0 {
			return names
		}
		name = name[i+1:]
	}
}
func newDNSState(base *dnsBase) dnsState {
	s := dnsState{names: make(map[string]bool)}
	for name, a := range base.names {
		s.names[name] = a
		s.memory += int64(len(name) + 64)
	}
	return s
}
func newDNSControl(base []byte, path string, address, commonAddress net.IP) (*dnsControl, error) {
	initial, err := parseDNSBase(base, address)
	if err != nil {
		return nil, err
	}
	if commonAddress.To4() == nil {
		return nil, fmt.Errorf("invalid common ingress IPv4 address")
	}
	copy(initial.commonAddress[:], commonAddress.To4())
	d := &dnsControl{base: initial, path: path, ops: defaultDNSFileOps(), queue: make(chan dnsUpdate, 128), syncWake: make(chan struct{}, 1), syncDone: make(chan struct{})}
	d.state = newDNSState(initial)
	if err := d.recover(); err != nil {
		return nil, err
	}
	d.syncedSeq = d.state.seq
	go d.syncJournalLoop()
	d.writer.Add(1)
	go d.run()
	return d, nil
}
func (d *dnsControl) reply() dnsReply {
	d.mu.RLock()
	defer d.mu.RUnlock()
	r := dnsReply{Epoch: d.state.epoch, Serial: d.state.serial(), status: http.StatusOK}
	if !d.state.ready || d.state.failed {
		r.Error = "DNS state unavailable"
		r.status = http.StatusServiceUnavailable
	}
	return r
}
func (d *dnsControl) fail() { d.mu.Lock(); d.state.failed = true; d.mu.Unlock() }
func (d *dnsControl) run() {
	defer d.writer.Done()
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
		if first.Reset {
			first.done <- d.reset()
			continue
		}
		batch := []dnsUpdate{first}
	collect:
		for len(batch) < 32 {
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
			default:
				break collect
			}
		}
		d.registerBatch(batch)
	}
}
func (d *dnsControl) registerBatch(batch []dnsUpdate) {
	results := make([]dnsReply, len(batch))
	d.mu.RLock()
	epoch, serial, seq := d.state.epoch, d.state.serial(), d.state.seq
	ready := d.state.ready && !d.state.failed
	dynamic, entries, memory := d.state.dynamic, len(d.state.names), d.state.memory
	staged := make(map[string]bool)
	var names []string
	for i, r := range batch {
		result := dnsReply{Epoch: epoch, Serial: serial, status: http.StatusOK}
		switch {
		case !ready:
			result.Error = "DNS state unavailable"
			result.status = 503
		case r.Epoch != epoch:
			result.Error = "stale DNS generation"
			result.status = 409
		default:
			name, err := dnsUserName(r.Name)
			if err != nil {
				result.Error = err.Error()
				result.status = 400
				break
			}
			if d.state.names[name] || staged[name] {
				break
			}
			var added []string
			var cost int64
			for _, key := range dnsAncestors(name) {
				if _, exists := d.state.names[key]; exists {
					continue
				}
				if _, exists := staged[key]; exists {
					continue
				}
				added = append(added, key)
				cost += int64(len(key) + 64)
			}
			if dynamic+1 > dnsDynamicLimit || entries+len(added) > dnsEntryLimit || memory+cost > dnsMemoryLimit || dnsHeaderSize+int64(seq+uint64(len(names)+1))*dnsRecordSize > dnsJournalLimit {
				result.Error = "DNS capacity reached"
				result.status = 503
				break
			}
			for _, key := range added {
				staged[key] = false
			}
			staged[name] = true
			names = append(names, name)
			dynamic++
			entries += len(added)
			memory += cost
		}
		results[i] = result
	}
	d.mu.RUnlock()
	if len(names) > 0 {
		data := make([]byte, len(names)*dnsRecordSize)
		for i, name := range names {
			encodeDNSRecord(data[i*dnsRecordSize:(i+1)*dnsRecordSize], seq+uint64(i)+1, name)
		}
		err := d.ops.write(d.file, data)
		if err != nil {
			d.fail()
			for i := range results {
				if results[i].Error == "" {
					results[i].Error = "DNS journal write: " + err.Error()
					results[i].status = 503
				}
			}
		} else {
			d.mu.Lock()
			for name, a := range staged {
				d.state.names[name] = a
			}
			d.state.seq += uint64(len(names))
			d.state.dynamic = dynamic
			d.state.memory = memory
			serial = d.state.serial()
			wake := (d.state.seq-d.syncedSeq)*dnsRecordSize >= dnsSyncBytes
			d.mu.Unlock()
			d.counters.updates.Add(uint64(len(names)))
			if wake {
				select {
				case d.syncWake <- struct{}{}:
				default:
				}
			}
		}
	}
	for i, r := range batch {
		if results[i].Error == "" {
			results[i].Serial = serial
		}
		r.done <- results[i]
	}
}
func (d *dnsControl) enqueue(ctx context.Context, r dnsUpdate) (dnsReply, error) {
	d.life.RLock()
	if d.closed {
		d.life.RUnlock()
		return dnsReply{}, errors.New("DNS service closed")
	}
	r.done = make(chan dnsReply, 1)
	select {
	case d.queue <- r:
		d.life.RUnlock()
	case <-ctx.Done():
		d.life.RUnlock()
		return dnsReply{}, ctx.Err()
	}
	select {
	case reply := <-r.done:
		return reply, nil
	case <-ctx.Done():
		return dnsReply{}, ctx.Err()
	}
}
func (d *dnsControl) close() error {
	d.life.Lock()
	if d.closed {
		d.life.Unlock()
		return nil
	}
	d.closed = true
	close(d.queue)
	d.life.Unlock()
	d.writer.Wait()
	close(d.syncWake)
	<-d.syncDone
	err := d.syncErr
	if d.file != nil {
		if e := d.file.Close(); err == nil {
			err = e
		}
	}
	return err
}
func (d *dnsControl) status() map[string]any {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return map[string]any{"ready": d.state.ready && !d.state.failed, "failed": d.state.failed, "epoch": d.state.epoch, "serial": d.state.serial(), "dynamic_names": d.state.dynamic, "entries": len(d.state.names), "model_budget_bytes": d.state.memory, "log_bytes": dnsHeaderSize + int64(d.state.seq)*dnsRecordSize, "published_seq": d.state.seq, "synced_seq": d.syncedSeq, "pending_bytes": (d.state.seq - d.syncedSeq) * dnsRecordSize, "updates": d.counters.updates.Load(), "syncs": d.counters.syncs.Load(), "sync_ns": d.counters.syncNanos.Load(), "queries": d.counters.queries.Load(), "answers": d.counters.answers.Load(), "nxdomain": d.counters.nxdomain.Load(), "nodata": d.counters.nodata.Load(), "send_errors": d.counters.sendErrors.Load()}
}
func (d *dnsControl) handler(allowed map[string]bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if !allowed[host] {
			http.Error(w, "forbidden", 403)
			return
		}
		if r.URL.Path == "/status" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(d.status())
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		var reply dnsReply
		switch r.URL.Path {
		case "/generation":
			reply = d.reply()
		case "/register", "/reset":
			update := dnsUpdate{Reset: r.URL.Path == "/reset"}
			if !update.Reset {
				if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&update); err != nil || !validDNSUser(update.Name) {
					http.Error(w, "invalid DNS name", 400)
					return
				}
				update.Reset = false
			}
			var err error
			reply, err = d.enqueue(r.Context(), update)
			if err != nil {
				http.Error(w, err.Error(), 503)
				return
			}
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_ = json.NewEncoder(w).Encode(reply)
	})
}
func runDNSControl() error {
	base, err := os.ReadFile("/etc/isupipe-dns/u.isucon.dev.zone")
	if err != nil {
		return err
	}
	address := net.ParseIP(os.Getenv(powerDNSSubdomainAddressEnvKey))
	if address.To4() == nil {
		return fmt.Errorf("invalid IPv4 DNS address")
	}
	commonAddress := net.ParseIP(os.Getenv("ISUPIPE_DNS_COMMON_ADDRESS"))
	d, err := newDNSControl(base, filepath.Join("/var/lib/isupipe/dns", "registrations.wal"), address, commonAddress)
	if err != nil {
		return err
	}
	defer func() {
		if err := d.close(); err != nil {
			fmt.Fprintf(os.Stderr, "DNS journal shutdown: %v\n", err)
		}
	}()
	httpAddr := os.Getenv("ISUPIPE_DNS_LISTEN")
	own, _, err := net.SplitHostPort(httpAddr)
	if err != nil {
		return err
	}
	allowed := map[string]bool{"127.0.0.1": true, own: true}
	for _, ip := range strings.Fields(os.Getenv("ISUPIPE_DNS_CLIENTS")) {
		allowed[ip] = true
	}
	listener, err := net.Listen("tcp", httpAddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 53})
	if err != nil {
		return err
	}
	defer udp.Close()
	server := &http.Server{Handler: d.handler(allowed), ReadHeaderTimeout: time.Second}
	errs := make(chan error, 2)
	go func() { errs <- server.Serve(listener) }()
	go func() { errs <- d.serveDNS(udp) }()
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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	select {
	case err := <-errs:
		server.Close()
		udp.Close()
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = server.Shutdown(shutdown)
	if err != nil {
		server.Close()
	}
	udp.Close()
	return err
}
func updateDNS(ctx context.Context, path, name string) error {
	body, err := json.Marshal(dnsUpdate{Name: name, Epoch: dnsGeneration.Load()})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, os.Getenv("ISUPIPE_DNS_URL")+path, strings.NewReader(string(body)))
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&reply); err != nil {
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
