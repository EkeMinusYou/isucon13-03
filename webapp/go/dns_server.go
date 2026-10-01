package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/net/dns/dnsmessage"
)

type dnsBase struct {
	names   map[string]bool
	address [4]byte
	ns      dnsmessage.NSResource
	soa     dnsmessage.SOAResource
}

func validDNSFQDN(name string) bool {
	if len(name) < 2 || len(name) > 254 || !strings.HasSuffix(name, ".") {
		return false
	}
	for _, label := range strings.Split(name[:len(name)-1], ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
	}
	return true
}
func inDNSZone(name string) bool { return name == dnsSuffix || strings.HasSuffix(name, "."+dnsSuffix) }
func parseDNSBase(data []byte, address net.IP) (*dnsBase, error) {
	ip := address.To4()
	if ip == nil {
		return nil, fmt.Errorf("DNS address must be IPv4")
	}
	b := &dnsBase{names: make(map[string]bool)}
	copy(b.address[:], ip)
	resolve := func(s string) string {
		if s == "@" {
			return dnsSuffix
		}
		if !strings.HasSuffix(s, ".") {
			s += "." + dnsSuffix
		}
		return dnsCanonical(s)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	var statement string
	soaSeen, nsSeen := false, false
	consume := func(line string) error {
		f := strings.Fields(line)
		if len(f) == 0 {
			return nil
		}
		if f[0] == "$TTL" {
			if len(f) != 2 {
				return fmt.Errorf("invalid TTL directive")
			}
			_, err := strconv.ParseUint(f[1], 10, 32)
			return err
		}
		name := resolve(f[0])
		if !validDNSFQDN(name) || !inDNSZone(name) {
			return fmt.Errorf("invalid base zone name %q", name)
		}
		if len(f) == 11 && f[1] == "SOA" && f[4] == "(" && f[10] == ")" {
			if soaSeen || name != dnsSuffix {
				return fmt.Errorf("duplicate or non-apex SOA")
			}
			ns, mbox := resolve(f[2]), resolve(f[3])
			if !validDNSFQDN(ns) || !validDNSFQDN(mbox) {
				return fmt.Errorf("invalid SOA names")
			}
			var values [5]uint32
			for i := range values {
				v, err := strconv.ParseUint(f[5+i], 10, 32)
				if err != nil {
					return err
				}
				values[i] = uint32(v)
			}
			b.soa = dnsmessage.SOAResource{NS: dnsmessage.MustNewName(ns), MBox: dnsmessage.MustNewName(mbox), Serial: values[0], Refresh: values[1], Retry: values[2], Expire: values[3], MinTTL: values[4]}
			soaSeen = true
		} else if len(f) == 5 && f[2] == "IN" {
			if _, err := strconv.ParseUint(f[1], 10, 32); err != nil {
				return err
			}
			switch f[3] {
			case "A":
				if f[4] != "<ISUCON_SUBDOMAIN_ADDRESS>" && net.ParseIP(f[4]).To4() == nil {
					return fmt.Errorf("invalid base IPv4 record")
				}
				for _, ancestor := range dnsAncestors(name) {
					if _, ok := b.names[ancestor]; !ok {
						b.names[ancestor] = false
					}
				}
				b.names[name] = true
			case "NS":
				target := resolve(f[4])
				if nsSeen || name != dnsSuffix || !validDNSFQDN(target) || !inDNSZone(target) {
					return fmt.Errorf("invalid base NS")
				}
				b.ns = dnsmessage.NSResource{NS: dnsmessage.MustNewName(target)}
				nsSeen = true
			default:
				return fmt.Errorf("unsupported base record %q", f[3])
			}
		} else {
			return fmt.Errorf("unsupported base zone statement %q", line)
		}
		if _, ok := b.names[name]; !ok {
			b.names[name] = false
		}
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), ";", 2)[0])
		if line == "" {
			continue
		}
		if statement != "" {
			statement += " " + line
		} else {
			statement = line
		}
		if strings.Contains(statement, "(") && !strings.Contains(statement, ")") {
			continue
		}
		if err := consume(statement); err != nil {
			return nil, err
		}
		statement = ""
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if statement != "" || !soaSeen || !nsSeen || !b.names[b.ns.NS.String()] {
		return nil, fmt.Errorf("incomplete base DNS zone")
	}
	return b, nil
}

// Respond without doing disk I/O or creating a goroutine per packet.
func (d *dnsControl) dnsResponse(packet, scratch []byte) []byte {
	if len(packet) < 12 || packet[2]&0x80 != 0 {
		return nil
	}
	d.counters.queries.Add(1)
	response := dnsmessage.Message{Header: dnsmessage.Header{ID: binary.BigEndian.Uint16(packet[:2]), Response: true, RecursionDesired: packet[2]&1 != 0}}
	malformed := func() []byte {
		response.Header.RCode = dnsmessage.RCodeFormatError
		response.Questions = nil
		response.Answers = nil
		response.Authorities = nil
		response.Additionals = nil
		b, _ := response.AppendPack(scratch[:0])
		return b
	}
	var p dnsmessage.Parser
	hdr, err := p.Start(packet)
	if err != nil {
		return malformed()
	}
	response.Header.OpCode = hdr.OpCode
	if hdr.OpCode != 0 {
		response.Header.RCode = dnsmessage.RCodeNotImplemented
		b, _ := response.AppendPack(scratch[:0])
		return b
	}
	question, err := p.Question()
	if err != nil {
		return malformed()
	}
	if _, err := p.Question(); err != dnsmessage.ErrSectionDone {
		return malformed()
	}
	if err := p.SkipAllAnswers(); err != nil {
		return malformed()
	}
	if err := p.SkipAllAuthorities(); err != nil {
		return malformed()
	}
	additional, err := p.AllAdditionals()
	if err != nil {
		return malformed()
	}
	response.Questions = []dnsmessage.Question{question}
	limit := 512
	edns := false
	badVersion := false
	for _, r := range additional {
		if r.Header.Type != dnsmessage.TypeOPT {
			continue
		}
		if edns || r.Header.Name.String() != "." {
			return malformed()
		}
		edns = true
		limit = int(r.Header.Class)
		if limit < 512 {
			limit = 512
		}
		if limit > 1232 {
			limit = 1232
		}
		badVersion = (r.Header.TTL>>16)&0xff != 0
	}
	if edns {
		ttl := uint32(0)
		if badVersion {
			ttl = 1 << 24
		}
		response.Additionals = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("."), Type: dnsmessage.TypeOPT, Class: 1232, TTL: ttl}, Body: &dnsmessage.OPTResource{}}}
	}
	if badVersion {
		return packDNSResponse(&response, scratch, limit)
	}
	if question.Class != dnsmessage.ClassINET {
		response.Header.RCode = dnsmessage.RCodeRefused
		return packDNSResponse(&response, scratch, limit)
	}
	name := dnsCanonical(question.Name.String())
	if !validDNSFQDN(name) || !inDNSZone(name) {
		response.Header.RCode = dnsmessage.RCodeRefused
		return packDNSResponse(&response, scratch, limit)
	}
	response.Header.Authoritative = true
	d.mu.RLock()
	if !d.state.ready || d.state.failed {
		d.mu.RUnlock()
		response.Header.RCode = dnsmessage.RCodeServerFailure
		return packDNSResponse(&response, scratch, limit)
	}
	a, exists := d.state.names[name]
	soa := d.base.soa
	soa.Serial = d.state.serial()
	d.mu.RUnlock()
	aRecord := func(owner dnsmessage.Name) dnsmessage.Resource {
		return dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: owner, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 300}, Body: &dnsmessage.AResource{A: d.base.address}}
	}
	soaRecord := func(ttl uint32) dnsmessage.Resource {
		return dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(dnsSuffix), Type: dnsmessage.TypeSOA, Class: dnsmessage.ClassINET, TTL: ttl}, Body: &soa}
	}
	if !exists {
		response.Header.RCode = dnsmessage.RCodeNameError
		response.Authorities = []dnsmessage.Resource{soaRecord(0)}
		d.counters.nxdomain.Add(1)
	} else {
		if a && (question.Type == dnsmessage.TypeA || question.Type == dnsmessage.TypeALL) {
			response.Answers = append(response.Answers, aRecord(question.Name))
			d.counters.answers.Add(1)
		}
		if name == dnsSuffix {
			if question.Type == dnsmessage.TypeNS || question.Type == dnsmessage.TypeALL {
				response.Answers = append(response.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeNS, Class: dnsmessage.ClassINET}, Body: &d.base.ns})
				response.Additionals = append(response.Additionals, aRecord(d.base.ns.NS))
			}
			if question.Type == dnsmessage.TypeSOA || question.Type == dnsmessage.TypeALL {
				response.Answers = append(response.Answers, soaRecord(3600))
			}
		}
		if len(response.Answers) == 0 {
			response.Authorities = []dnsmessage.Resource{soaRecord(0)}
			d.counters.nodata.Add(1)
		}
	}
	return packDNSResponse(&response, scratch, limit)
}
func packDNSResponse(m *dnsmessage.Message, scratch []byte, limit int) []byte {
	for {
		b, err := m.AppendPack(scratch[:0])
		if err != nil {
			return nil
		}
		if len(b) <= limit {
			return b
		}
		m.Header.Truncated = true
		// Remove entire records; OPT remains when it is the sole additional record.
		if len(m.Additionals) > 0 && m.Additionals[len(m.Additionals)-1].Header.Type != dnsmessage.TypeOPT {
			m.Additionals = m.Additionals[:len(m.Additionals)-1]
		} else if len(m.Authorities) > 0 {
			m.Authorities = m.Authorities[:len(m.Authorities)-1]
		} else if len(m.Answers) > 0 {
			m.Answers = m.Answers[:len(m.Answers)-1]
		} else {
			return nil
		}
	}
}
func (d *dnsControl) serveDNS(conn *net.UDPConn) error {
	var workers sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			input := make([]byte, 4096)
			scratch := make([]byte, 0, 1232)
			for {
				n, _, flags, from, err := conn.ReadMsgUDPAddrPort(input, nil)
				if err != nil {
					errs <- err
					return
				}
				if flags&syscall.MSG_TRUNC != 0 {
					continue
				}
				answer := d.dnsResponse(input[:n], scratch)
				if len(answer) == 0 {
					continue
				}
				if _, err := conn.WriteToUDPAddrPort(answer, from); err != nil {
					d.counters.sendErrors.Add(1)
				}
			}
		}()
	}
	err := <-errs
	conn.Close()
	workers.Wait()
	return err
}
