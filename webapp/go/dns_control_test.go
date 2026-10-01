package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const dnsTestBase = "$TTL 3600\n@ SOA ns1 hostmaster.u.isucon.dev. ( 0 10800 3600 604800 3600 )\n@ 0 IN NS ns1.u.isucon.dev.\n@ 0 IN A <ISUCON_SUBDOMAIN_ADDRESS>\nns1 0 IN A <ISUCON_SUBDOMAIN_ADDRESS>\npipe 0 IN A <ISUCON_SUBDOMAIN_ADDRESS>\n"

func TestDNSSplitIngressAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registrations.wal")
	streamIP, commonIP := net.ParseIP("192.0.2.1"), net.ParseIP("192.0.2.2")
	d, err := newDNSControl([]byte(dnsTestBase), path, streamIP, commonIP)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.close() })
	initial := dnsCall(t, d, dnsUpdate{Reset: true})
	registered := dnsCall(t, d, dnsUpdate{Name: "child.parent", Epoch: initial.Epoch})
	assertRoutes := func() {
		t.Helper()
		for _, tc := range []struct {
			name string
			ip   [4]byte
		}{
			{"PIPE." + dnsSuffix, [4]byte{192, 0, 2, 2}},
			{"child.parent." + dnsSuffix, [4]byte{192, 0, 2, 1}},
			{"ns1." + dnsSuffix, [4]byte{192, 0, 2, 1}},
		} {
			answer := dnsQuestion(t, d, tc.name, dnsmessage.TypeA)
			if answer.RCode != 0 || len(answer.Answers) != 1 || answer.Answers[0].Header.TTL != 300 || answer.Answers[0].Body.(*dnsmessage.AResource).A != tc.ip {
				t.Fatalf("%s: wrong ingress: %+v", tc.name, answer)
			}
		}
		if r := dnsQuestion(t, d, "missing."+dnsSuffix, dnsmessage.TypeA); r.RCode != dnsmessage.RCodeNameError {
			t.Fatal("unknown name acquired an ingress")
		}
		if r := dnsQuestion(t, d, "parent."+dnsSuffix, dnsmessage.TypeA); r.RCode != 0 || len(r.Answers) != 0 {
			t.Fatal("empty nonterminal acquired an ingress")
		}
		if r := dnsQuestion(t, d, "pipe."+dnsSuffix, dnsmessage.TypeAAAA); len(r.Answers) != 0 || len(r.Authorities) != 1 {
			t.Fatal("AAAA no-data changed")
		}
		if r := dnsQuestion(t, d, dnsSuffix, dnsmessage.TypeNS); len(r.Additionals) != 1 || r.Additionals[0].Body.(*dnsmessage.AResource).A != [4]byte{192, 0, 2, 1} {
			t.Fatal("NS glue moved to the common HTTP ingress")
		}
	}
	assertRoutes()
	d.close()
	d, err = newDNSControl([]byte(dnsTestBase), path, streamIP, commonIP)
	if err != nil {
		t.Fatal(err)
	}
	if r := d.reply(); r.Epoch != registered.Epoch || r.Serial != registered.Serial {
		t.Fatal("ingress split changed recovered registration state")
	}
	assertRoutes()
}

func testDNS(t *testing.T) *dnsControl {
	t.Helper()
	d, err := newDNSControl([]byte(dnsTestBase), filepath.Join(t.TempDir(), "registrations.wal"), net.ParseIP("192.0.2.1"), net.ParseIP("192.0.2.1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.close() })
	return d
}
func dnsCall(t *testing.T, d *dnsControl, r dnsUpdate) dnsReply {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := d.enqueue(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func dnsQuestion(t *testing.T, d *dnsControl, name string, kind dnsmessage.Type) dnsmessage.Message {
	t.Helper()
	query := dnsmessage.Message{Header: dnsmessage.Header{ID: 42, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: kind, Class: dnsmessage.ClassINET}}}
	return dnsPacket(t, d, query)
}
func dnsPacket(t *testing.T, d *dnsControl, query dnsmessage.Message) dnsmessage.Message {
	t.Helper()
	wire, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	answer := d.dnsResponse(wire, make([]byte, 0, 1232))
	var result dnsmessage.Message
	if err := result.Unpack(answer); err != nil {
		t.Fatal(err)
	}
	return result
}
func TestDNSPublishAfterSyncAndRecovery(t *testing.T) {
	d := testDNS(t)
	if r := dnsQuestion(t, d, "pipe."+dnsSuffix, dnsmessage.TypeA); r.RCode != dnsmessage.RCodeServerFailure {
		t.Fatal("uninitialized DNS answered")
	}
	initial := dnsCall(t, d, dnsUpdate{Reset: true})
	if initial.status != 200 {
		t.Fatal(initial)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	syncFile := d.ops.sync
	d.ops.sync = func(f *os.File) error { close(entered); <-release; return syncFile(f) }
	done := make(chan dnsReply, 1)
	go func() {
		r, _ := d.enqueue(context.Background(), dnsUpdate{Name: "Child.Parent", Epoch: initial.Epoch})
		done <- r
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("sync not reached")
	}
	before := dnsQuestion(t, d, "child.parent."+dnsSuffix, dnsmessage.TypeA)
	if before.RCode != dnsmessage.RCodeNameError {
		t.Error("published before durable sync")
	}
	select {
	case <-done:
		t.Error("acknowledged before durable sync")
	default:
	}
	close(release)
	result := <-done
	d.ops.sync = syncFile
	if result.status != 200 || result.Serial != initial.Serial+1 {
		t.Fatal(result)
	}
	after := dnsQuestion(t, d, "CHILD.PARENT."+dnsSuffix, dnsmessage.TypeA)
	if after.RCode != 0 || len(after.Answers) != 1 || after.Answers[0].Header.TTL != 300 {
		t.Fatal(after)
	}
	parent := dnsQuestion(t, d, "parent."+dnsSuffix, dnsmessage.TypeA)
	if parent.RCode != 0 || len(parent.Answers) != 0 || len(parent.Authorities) != 1 {
		t.Fatal("empty nonterminal became NXDOMAIN or A")
	}
	if duplicate := dnsCall(t, d, dnsUpdate{Name: "child.parent", Epoch: initial.Epoch}); duplicate.Serial != result.Serial {
		t.Fatal("duplicate appended")
	}
	d.close()
	restored, err := newDNSControl([]byte(dnsTestBase), d.path, net.ParseIP("192.0.2.2"), net.ParseIP("192.0.2.2"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	if r := restored.reply(); r.Epoch != result.Epoch || r.Serial != result.Serial {
		t.Fatal("restart changed generation", r)
	}
	answer := dnsQuestion(t, restored, "child.parent."+dnsSuffix, dnsmessage.TypeA)
	if answer.Answers[0].Body.(*dnsmessage.AResource).A != [4]byte{192, 0, 2, 2} {
		t.Fatal("restart retained old address")
	}
	reset := dnsCall(t, restored, dnsUpdate{Reset: true})
	if reset.status != 200 || reset.Epoch != result.Epoch+1 {
		t.Fatal(reset)
	}
	if stale := dnsCall(t, restored, dnsUpdate{Name: "late", Epoch: result.Epoch}); stale.status != 409 {
		t.Fatal("stale generation accepted")
	}
	if r := dnsQuestion(t, restored, "child.parent."+dnsSuffix, dnsmessage.TypeA); r.RCode != dnsmessage.RCodeNameError {
		t.Fatal("reset retained dynamic name")
	}
	if r := dnsQuestion(t, restored, "pipe."+dnsSuffix, dnsmessage.TypeA); len(r.Answers) != 1 {
		t.Fatal("reset lost initial zone")
	}
}
func TestDNSIOFailures(t *testing.T) {
	for _, failure := range []string{"write", "sync", "rename", "directory"} {
		t.Run(failure, func(t *testing.T) {
			d := testDNS(t)
			initial := dnsCall(t, d, dnsUpdate{Reset: true})
			ops := d.ops
			switch failure {
			case "write":
				d.ops.write = func(*os.File, []byte) error { return errors.New("injected write") }
			case "sync":
				d.ops.sync = func(*os.File) error { return errors.New("injected sync") }
			case "rename":
				d.ops.rename = func(string, string) error { return errors.New("injected rename") }
			case "directory":
				d.ops.syncDir = func(string) error { return errors.New("injected directory sync") }
			}
			request := dnsUpdate{Name: "failed", Epoch: initial.Epoch}
			if failure == "rename" || failure == "directory" {
				request = dnsUpdate{Reset: true}
			}
			if r := dnsCall(t, d, request); r.status != 503 {
				t.Fatal("failure acknowledged", r)
			}
			if r := dnsQuestion(t, d, "pipe."+dnsSuffix, dnsmessage.TypeA); r.RCode != dnsmessage.RCodeServerFailure {
				t.Fatal("failed DNS remained available")
			}
			d.ops = ops
			if r := dnsCall(t, d, dnsUpdate{Name: "another", Epoch: initial.Epoch}); r.status != 503 {
				t.Fatal("failed state accepted registration")
			}
			if r := dnsCall(t, d, dnsUpdate{Reset: true}); r.status != 200 {
				t.Fatal("explicit reset failed to recover", r)
			}
		})
	}
}
func TestDNSJournalTornTailAndCorruption(t *testing.T) {
	d := testDNS(t)
	epoch := dnsCall(t, d, dnsUpdate{Reset: true}).Epoch
	dnsCall(t, d, dnsUpdate{Name: "durable", Epoch: epoch})
	d.close()
	original, err := os.ReadFile(d.path)
	if err != nil {
		t.Fatal(err)
	}
	tail := make([]byte, dnsRecordSize)
	encodeDNSRecord(tail, 2, "unacknowledged."+dnsSuffix)
	for _, size := range []int{1, 100, dnsRecordSize - 1} {
		path := filepath.Join(t.TempDir(), "journal")
		os.WriteFile(path, append(bytes.Clone(original), tail[:size]...), 0640)
		r, err := newDNSControl([]byte(dnsTestBase), path, net.ParseIP("192.0.2.1"), net.ParseIP("192.0.2.1"))
		if err != nil {
			t.Fatal(err)
		}
		r.close()
		stat, _ := os.Stat(path)
		if stat.Size() != int64(len(original)) {
			t.Fatal("torn tail not truncated")
		}
	}
	for _, offset := range []int{0, 28, dnsHeaderSize + 10, dnsHeaderSize + 284} {
		path := filepath.Join(t.TempDir(), "journal")
		corrupted := bytes.Clone(original)
		corrupted[offset] ^= 1
		os.WriteFile(path, corrupted, 0640)
		if r, err := newDNSControl([]byte(dnsTestBase), path, net.ParseIP("192.0.2.1"), net.ParseIP("192.0.2.1")); err == nil {
			r.close()
			t.Fatal("corruption silently accepted", offset)
		}
	}
}
func TestDNSResponses(t *testing.T) {
	d := testDNS(t)
	dnsCall(t, d, dnsUpdate{Reset: true})
	for _, tc := range []struct {
		name      string
		kind      dnsmessage.Type
		code      dnsmessage.RCode
		answers   int
		authority int
	}{
		{"pipe." + dnsSuffix, dnsmessage.TypeA, 0, 1, 0}, {"pipe." + dnsSuffix, dnsmessage.TypeAAAA, 0, 0, 1}, {"missing." + dnsSuffix, dnsmessage.TypeA, dnsmessage.RCodeNameError, 0, 1}, {dnsSuffix, dnsmessage.TypeNS, 0, 1, 0}, {dnsSuffix, dnsmessage.TypeSOA, 0, 1, 0}, {dnsSuffix, dnsmessage.TypeALL, 0, 3, 0}, {"outside.example.", dnsmessage.TypeA, dnsmessage.RCodeRefused, 0, 0},
	} {
		r := dnsQuestion(t, d, tc.name, tc.kind)
		if r.ID != 42 || !r.Response || !r.RecursionDesired || r.RecursionAvailable || r.AuthenticData || r.RCode != tc.code || len(r.Answers) != tc.answers || len(r.Authorities) != tc.authority {
			t.Fatal(tc, r)
		}
		if len(r.Authorities) > 0 && r.Authorities[0].Header.TTL != 0 {
			t.Fatal("negative TTL must be zero")
		}
	}
	query := dnsmessage.Message{Header: dnsmessage.Header{ID: 9}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("pipe." + dnsSuffix), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}, Additionals: []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("."), Type: dnsmessage.TypeOPT, Class: 4096, TTL: 1 << 16}, Body: &dnsmessage.OPTResource{}}}}
	r := dnsPacket(t, d, query)
	if r.RCode != 0 || len(r.Answers) != 0 || len(r.Additionals) != 1 || r.Additionals[0].Header.TTL>>24 != 1 {
		t.Fatal("BADVERS response", r)
	}
	query.Additionals[0].Header.TTL = 0
	r = dnsPacket(t, d, query)
	if len(r.Answers) != 1 || r.Additionals[0].Header.Class != 1232 {
		t.Fatal("EDNS response", r)
	}
	query.Additionals = nil
	query.Questions = append(query.Questions, query.Questions[0])
	if r := dnsPacket(t, d, query); r.RCode != dnsmessage.RCodeFormatError {
		t.Fatal("multiple questions accepted")
	}
	query.Questions = query.Questions[:1]
	query.Questions[0].Class = dnsmessage.ClassCHAOS
	if r := dnsPacket(t, d, query); r.RCode != dnsmessage.RCodeRefused {
		t.Fatal("non-IN accepted")
	}
	query.Header.OpCode = 2
	if r := dnsPacket(t, d, query); r.RCode != dnsmessage.RCodeNotImplemented {
		t.Fatal("non-query opcode accepted")
	}
	if r := d.dnsResponse([]byte{1, 2}, nil); r != nil {
		t.Fatal("short packet answered")
	}
}
func TestDNSHTTPAndNames(t *testing.T) {
	d := testDNS(t)
	server := httptest.NewServer(d.handler(map[string]bool{"127.0.0.1": true}))
	defer server.Close()
	response, err := http.Post(server.URL+"/generation", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 503 {
		t.Fatal("uninitialized generation accepted")
	}
	initial := dnsCall(t, d, dnsUpdate{Reset: true})
	for _, name := range []string{"test001", "a-b", "nested.name", "under_score", "日本語", "a b", "a\nb 0 IN A 1.2.3.4"} {
		body, _ := json.Marshal(dnsUpdate{Name: name, Epoch: initial.Epoch})
		response, err := http.Post(server.URL+"/register", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("rejected", name)
		}
		if r := dnsQuestion(t, d, dnsCanonical(name)+"."+dnsSuffix, dnsmessage.TypeA); len(r.Answers) != 1 {
			t.Fatal("name did not resolve", name)
		}
	}
	for _, name := range []string{"", "a..b", strings.Repeat("x", 64), strings.Repeat("a.", 125) + "a"} {
		if validDNSUser(name) {
			t.Fatal("accepted invalid name", name)
		}
	}
	denied := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/status", nil)
	request.RemoteAddr = "192.0.2.99:1234"
	d.handler(map[string]bool{"127.0.0.1": true}).ServeHTTP(denied, request)
	if denied.Code != 403 {
		t.Fatal("private API permitted foreign client")
	}
}
func TestDNSOfficialZone(t *testing.T) {
	data, err := os.ReadFile("../pdns/u.isucon.dev.zone")
	if err != nil {
		t.Fatal(err)
	}
	base, err := parseDNSBase(data, net.ParseIP("192.0.2.1"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, a := range base.names {
		if a {
			count++
		}
	}
	if count != 1278 {
		t.Fatal("initial names lost", count)
	}
	if _, err := parseDNSBase(append(data, []byte("bad 0 IN TXT unsupported\n")...), net.ParseIP("192.0.2.1")); err == nil {
		t.Fatal("unsupported base record ignored")
	}
}
func TestDNSCrashRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal")
	cmd := exec.Command(os.Args[0], "-test.run=^TestDNSCrashHelper$")
	cmd.Env = append(os.Environ(), "DNS_CRASH_TEST_PATH="+path)
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	done := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(output).ReadString('\n'); done <- line }()
	select {
	case line := <-done:
		if line != "durable\n" {
			t.Fatal(line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child not ready")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	d, err := newDNSControl([]byte(dnsTestBase), path, net.ParseIP("192.0.2.1"), net.ParseIP("192.0.2.1"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	if r := dnsQuestion(t, d, "crash-durable."+dnsSuffix, dnsmessage.TypeA); len(r.Answers) != 1 {
		t.Fatal("SIGKILL lost acknowledged registration")
	}
}
func TestDNSCrashHelper(t *testing.T) {
	path := os.Getenv("DNS_CRASH_TEST_PATH")
	if path == "" {
		return
	}
	d, err := newDNSControl([]byte(dnsTestBase), path, net.ParseIP("192.0.2.1"), net.ParseIP("192.0.2.1"))
	if err != nil {
		t.Fatal(err)
	}
	epoch := dnsCall(t, d, dnsUpdate{Reset: true}).Epoch
	if r := dnsCall(t, d, dnsUpdate{Name: "crash-durable", Epoch: epoch}); r.status != 200 {
		t.Fatal(r)
	}
	fmt.Println("durable")
	select {}
}

func TestDNSResetQueueBoundaryAndCanceledRequest(t *testing.T) {
	d := testDNS(t)
	initial := dnsCall(t, d, dnsUpdate{Reset: true})
	entered, release := make(chan struct{}), make(chan struct{})
	syncFile := d.ops.sync
	var once sync.Once
	d.ops.sync = func(f *os.File) error { once.Do(func() { close(entered); <-release }); return syncFile(f) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := d.enqueue(ctx, dnsUpdate{Name: "accepted", Epoch: initial.Epoch}); done <- err }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	reset := dnsUpdate{Reset: true, done: make(chan dnsReply, 1)}
	stale := dnsUpdate{Name: "late", Epoch: initial.Epoch, done: make(chan dnsReply, 1)}
	current := dnsUpdate{Name: "fresh", Epoch: initial.Epoch + 1, done: make(chan dnsReply, 1)}
	d.queue <- reset
	d.queue <- stale
	d.queue <- current
	close(release)
	if r := <-reset.done; r.status != 200 || r.Epoch != initial.Epoch+1 {
		t.Fatal(r)
	}
	if r := <-stale.done; r.status != 409 {
		t.Fatal("reset crossed queued registration", r)
	}
	if r := <-current.done; r.status != 200 {
		t.Fatal(r)
	}
	if d.counters.updates.Load() != 2 {
		t.Fatal("accepted request canceled durable write")
	}
	if r := dnsQuestion(t, d, "accepted."+dnsSuffix, dnsmessage.TypeA); r.RCode != dnsmessage.RCodeNameError {
		t.Fatal("old registration survived reset")
	}
	if r := dnsQuestion(t, d, "fresh."+dnsSuffix, dnsmessage.TypeA); len(r.Answers) != 1 {
		t.Fatal("new generation missing")
	}
}
func TestDNSBatchDuplicatesAndCapacity(t *testing.T) {
	d := testDNS(t)
	epoch := dnsCall(t, d, dnsUpdate{Reset: true}).Epoch
	var batch []dnsUpdate
	for _, name := range []string{"child.parent", "parent", "CHILD.PARENT", "second"} {
		batch = append(batch, dnsUpdate{Name: name, Epoch: epoch, done: make(chan dnsReply, 1)})
	}
	d.registerBatch(batch)
	for _, r := range batch {
		if result := <-r.done; result.status != 200 {
			t.Fatal(result)
		}
	}
	if d.counters.syncs.Load() != 1 || d.counters.updates.Load() != 3 {
		t.Fatal("batch did not deduplicate and share sync")
	}
	if r := dnsQuestion(t, d, "parent."+dnsSuffix, dnsmessage.TypeA); len(r.Answers) != 1 {
		t.Fatal("ancestor promotion lost")
	}
	for _, limit := range []string{"names", "entries", "memory", "journal"} {
		t.Run(limit, func(t *testing.T) {
			d := testDNS(t)
			epoch := dnsCall(t, d, dnsUpdate{Reset: true}).Epoch
			d.mu.Lock()
			switch limit {
			case "names":
				d.state.dynamic = dnsDynamicLimit
			case "entries":
				for i := len(d.state.names); i < dnsEntryLimit; i++ {
					d.state.names[fmt.Sprintf("reserved-%d", i)] = false
				}
			case "memory":
				d.state.memory = dnsMemoryLimit
			case "journal":
				d.state.seq = dnsJournalLimit / dnsRecordSize
			}
			d.mu.Unlock()
			if r := dnsCall(t, d, dnsUpdate{Name: "overcapacity", Epoch: epoch}); r.status != 503 {
				t.Fatal("capacity not enforced", r)
			}
			if d.counters.syncs.Load() != 0 {
				t.Fatal("capacity rejection wrote to disk")
			}
		})
	}
}
func TestDNSUDPWorkers(t *testing.T) {
	d := testDNS(t)
	dnsCall(t, d, dnsUpdate{Reset: true})
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.serveDNS(conn) }()
	defer func() { conn.Close(); <-done }()
	client, err := net.DialUDP("udp4", nil, conn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	query := dnsmessage.Message{Header: dnsmessage.Header{ID: 65530}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("pipe." + dnsSuffix), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	wire, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(wire); err != nil {
		t.Fatal(err)
	}
	packet := make([]byte, 1232)
	n, err := client.Read(packet)
	if err != nil {
		t.Fatal(err)
	}
	var response dnsmessage.Message
	if err := response.Unpack(packet[:n]); err != nil {
		t.Fatal(err)
	}
	if response.ID != 65530 || len(response.Answers) != 1 {
		t.Fatal(response)
	}
}
