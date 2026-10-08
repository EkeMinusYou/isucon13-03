package main

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"time"
)

const dnsHeaderSize = 32
const dnsRecordSize = 288
const dnsDynamicLimit = 100000
const dnsEntryLimit = 200000
const dnsMemoryLimit = 64 << 20
const dnsJournalLimit = 32 << 20

var dnsCRC = crc32.MakeTable(crc32.Castagnoli)

type dnsFileOps struct {
	write   func(*os.File, []byte) error
	sync    func(*os.File) error
	rename  func(string, string) error
	syncDir func(string) error
}

func defaultDNSFileOps() dnsFileOps {
	return dnsFileOps{write: func(f *os.File, b []byte) error {
		for len(b) > 0 {
			n, err := f.Write(b)
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			b = b[n:]
		}
		return nil
	}, sync: func(f *os.File) error { return f.Sync() }, rename: os.Rename, syncDir: func(path string) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Sync()
	}}
}
func encodeDNSHeader(epoch uint64, serial uint32) []byte {
	b := make([]byte, dnsHeaderSize)
	copy(b, "ISUDNS01")
	binary.LittleEndian.PutUint64(b[8:16], epoch)
	binary.LittleEndian.PutUint32(b[16:20], serial)
	binary.LittleEndian.PutUint32(b[20:24], dnsRecordSize)
	binary.LittleEndian.PutUint32(b[28:32], crc32.Checksum(b[:28], dnsCRC))
	return b
}
func encodeDNSRecord(b []byte, seq uint64, name string) {
	clear(b)
	binary.LittleEndian.PutUint64(b[:8], seq)
	binary.LittleEndian.PutUint16(b[8:10], uint16(len(name)))
	copy(b[10:264], name)
	binary.LittleEndian.PutUint32(b[284:288], crc32.Checksum(b[:284], dnsCRC))
}
func (d *dnsControl) recover() error {
	f, err := os.OpenFile(d.path, os.O_RDWR, 0640)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if stat.Size() > dnsJournalLimit {
		return fmt.Errorf("DNS journal exceeds capacity")
	}
	header := make([]byte, dnsHeaderSize)
	if _, err := io.ReadFull(f, header); err != nil {
		return fmt.Errorf("DNS journal header: %w", err)
	}
	if string(header[:8]) != "ISUDNS01" || binary.LittleEndian.Uint32(header[20:24]) != dnsRecordSize || binary.LittleEndian.Uint32(header[24:28]) != 0 || crc32.Checksum(header[:28], dnsCRC) != binary.LittleEndian.Uint32(header[28:32]) {
		return fmt.Errorf("invalid DNS journal header")
	}
	d.state.epoch = binary.LittleEndian.Uint64(header[8:16])
	if d.state.epoch == 0 {
		return fmt.Errorf("zero DNS epoch")
	}
	d.state.baseSerial = binary.LittleEndian.Uint32(header[16:20])
	b := make([]byte, dnsRecordSize)
	for {
		n, err := io.ReadFull(f, b)
		if err == io.EOF {
			break
		}
		if err == io.ErrUnexpectedEOF {
			offset := int64(dnsHeaderSize) + int64(d.state.seq)*dnsRecordSize
			if err := f.Truncate(offset); err != nil {
				return err
			}
			if err := f.Sync(); err != nil {
				return err
			}
			break
		}
		if err != nil {
			return err
		}
		if n != dnsRecordSize {
			return io.ErrUnexpectedEOF
		}
		seq := binary.LittleEndian.Uint64(b[:8])
		size := int(binary.LittleEndian.Uint16(b[8:10]))
		if seq != d.state.seq+1 || size == 0 || size > 254 || crc32.Checksum(b[:284], dnsCRC) != binary.LittleEndian.Uint32(b[284:288]) {
			return fmt.Errorf("invalid DNS record at sequence %d", d.state.seq+1)
		}
		for _, v := range b[10+size : 284] {
			if v != 0 {
				return fmt.Errorf("nonzero DNS record padding")
			}
		}
		name := string(b[10 : 10+size])
		if !validDNSFQDN(name) || dnsCanonical(name) != name || !inDNSZone(name) || d.state.names[name] {
			return fmt.Errorf("invalid or duplicate DNS journal name")
		}
		if d.state.dynamic >= dnsDynamicLimit {
			return fmt.Errorf("DNS name capacity reached")
		}
		for _, key := range dnsAncestors(name) {
			if _, exists := d.state.names[key]; !exists {
				d.state.names[key] = false
				d.state.memory += int64(len(key) + 64)
			}
		}
		d.state.names[name] = true
		d.state.dynamic++
		d.state.seq = seq
		if len(d.state.names) > dnsEntryLimit || d.state.memory > dnsMemoryLimit {
			return fmt.Errorf("DNS memory capacity reached")
		}
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	d.state.ready = true
	d.file = f
	ok = true
	return nil
}
func (d *dnsControl) reset() dnsReply {
	// A generation change waits for an in-flight sync before replacing its file.
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	d.mu.RLock()
	epoch := d.state.epoch
	serial := d.state.serial() + 1
	d.mu.RUnlock()
	if epoch == 0 || epoch == ^uint64(0) {
		old := epoch
		var nonce [8]byte
		for {
			if _, err := rand.Read(nonce[:]); err != nil {
				return d.resetError(err)
			}
			epoch = binary.LittleEndian.Uint64(nonce[:])
			if epoch != 0 && epoch != old {
				break
			}
		}
		serial = uint32(time.Now().Unix())
	} else {
		epoch++
	}
	dir := filepath.Dir(d.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return d.resetError(err)
	}
	f, err := os.CreateTemp(dir, ".dns-reset-*")
	if err != nil {
		return d.resetError(err)
	}
	installed := false
	defer func() {
		if !installed {
			f.Close()
		}
		os.Remove(f.Name())
	}()
	if err = f.Chmod(0640); err == nil {
		err = d.ops.write(f, encodeDNSHeader(epoch, serial))
	}
	if err == nil {
		err = d.ops.sync(f)
	}
	if err == nil {
		err = d.ops.rename(f.Name(), d.path)
	}
	if err == nil {
		err = d.ops.syncDir(dir)
	}
	if err != nil {
		return d.resetError(err)
	}
	old := d.file
	d.file = f
	installed = true
	state := newDNSState(d.base)
	state.epoch = epoch
	state.baseSerial = serial
	state.ready = true
	d.mu.Lock()
	d.state = state
	d.syncedSeq = 0
	d.mu.Unlock()
	d.syncErr = nil
	if old != nil {
		old.Close()
	}
	return dnsReply{Epoch: epoch, Serial: serial, status: 200}
}

// Registration publication never waits on this worker. Close drains it after
// the writer has completed every accepted file write.
func (d *dnsControl) syncJournalLoop() {
	defer close(d.syncDone)
	ticker := time.NewTicker(dnsSyncInterval)
	defer ticker.Stop()
	for {
		closing := false
		select {
		case _, ok := <-d.syncWake:
			closing = !ok
		case <-ticker.C:
		}
		d.syncJournal()
		if closing {
			return
		}
	}
}

func (d *dnsControl) syncJournal() {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	if d.syncErr != nil || d.file == nil {
		return
	}
	d.mu.RLock()
	seq, synced := d.state.seq, d.syncedSeq
	d.mu.RUnlock()
	if seq == synced {
		return
	}
	start := time.Now()
	err := d.ops.sync(d.file)
	d.counters.syncNanos.Add(uint64(time.Since(start)))
	d.counters.syncs.Add(1)
	if err != nil {
		d.syncErr = fmt.Errorf("DNS journal sync: %w", err)
		d.fail()
		return
	}
	d.mu.Lock()
	d.syncedSeq = seq
	d.mu.Unlock()
}
func (d *dnsControl) resetError(err error) dnsReply {
	d.fail()
	r := d.reply()
	r.Error = "DNS reset: " + err.Error()
	r.status = 503
	return r
}
