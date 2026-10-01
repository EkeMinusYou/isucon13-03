package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

const postMagic = "ISUPWAL3"
const postHeaderSize = len(postMagic) + 36 + 8
const postFrameLimit = 1 << 20
const postFrameHeaderSize = 16
const postFrameFooter = "WALDONE3"
const postFrameFooterSize = len(postFrameFooter) + postFrameHeaderSize
const postEndSize = postFrameHeaderSize

var postCRC = crc32.MakeTable(crc32.Castagnoli)
var errPostFrameHeader = errors.New("WAL frame header checksum or size mismatch")

type postRecord struct {
	Seq       uint64                  `json:"seq"`
	Kind      string                  `json:"kind"`
	ID        int64                   `json:"id"`
	UserID    int64                   `json:"user"`
	StreamID  int64                   `json:"stream"`
	Text      string                  `json:"text"`
	Tip       int64                   `json:"tip,omitempty"`
	CreatedAt int64                   `json:"created"`
	Deleted   []int64                 `json:"deleted,omitempty"`
	Report    *LivecommentReportModel `json:"report,omitempty"`
}

func (r postRecord) cost() int64 {
	switch r.Kind {
	case "exit":
		return 0
	case "report":
		if r.Report != nil {
			return int64(512 + len(r.Report.Comment))
		}
	}
	return int64(256 + len(r.Text))
}
func (r postRecord) comment() LivecommentModel {
	return LivecommentModel{ID: r.ID, UserID: r.UserID, LivestreamID: r.StreamID, Comment: r.Text, Tip: r.Tip, CreatedAt: r.CreatedAt}
}
func (r postRecord) reaction() ReactionModel {
	return ReactionModel{ID: r.ID, UserID: r.UserID, LivestreamID: r.StreamID, EmojiName: r.Text, CreatedAt: r.CreatedAt}
}
func commentRecord(r LivecommentModel) postRecord {
	return postRecord{Kind: "comment", ID: r.ID, UserID: r.UserID, StreamID: r.LivestreamID, Text: r.Comment, Tip: r.Tip, CreatedAt: r.CreatedAt}
}
func reactionRecord(r ReactionModel) postRecord {
	return postRecord{Kind: "reaction", ID: r.ID, UserID: r.UserID, StreamID: r.LivestreamID, Text: r.EmojiName, CreatedAt: r.CreatedAt}
}

type postRequest struct {
	record postRecord
	done   chan postResult
}
type postResult struct {
	record postRecord
	err    error
}
type postDBState struct {
	Generation string `db:"generation"`
	Ready      bool   `db:"ready"`
}

func readPostState(ctx context.Context, db *sqlx.DB) (postDBState, error) {
	var state postDBState
	err := db.GetContext(ctx, &state, "SELECT generation,ready FROM post_wal_state WHERE id=1")
	return state, err
}

func createPostGeneration(ctx context.Context, db *sqlx.DB, dir string) (string, error) {
	return createPostGenerationCapacity(ctx, db, dir, postWALLimit)
}

func createPostGenerationCapacity(ctx context.Context, db *sqlx.DB, dir string, capacity int64) (string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	gen := uuid.NewString()
	file, err := os.OpenFile(filepath.Join(dir, gen+".wal"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
	if err != nil {
		return "", err
	}
	err = initializePostFile(ctx, file, gen, capacity)
	if err == nil {
		err = file.Sync()
	}
	if e := file.Close(); err == nil {
		err = e
	}
	if err != nil {
		return "", err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	err = directory.Sync()
	directory.Close()
	if err != nil {
		return "", err
	}
	result, err := db.ExecContext(ctx, "UPDATE post_wal_state SET generation=?,ready=TRUE WHERE id=1 AND ready=FALSE", gen)
	if err != nil {
		return "", err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if count != 1 {
		return "", fmt.Errorf("post generation requires unready database")
	}
	return gen, nil
}

func initializePostFile(ctx context.Context, file *os.File, generation string, capacity int64) error {
	if len(generation) != 36 || capacity < int64(postHeaderSize+postEndSize) || capacity > postWALLimit {
		return fmt.Errorf("invalid WAL generation or capacity")
	}
	header := make([]byte, postHeaderSize)
	copy(header, postMagic+generation)
	binary.LittleEndian.PutUint64(header[postHeaderSize-8:], uint64(capacity))
	if err := writePostBytes(file, header); err != nil {
		return err
	}
	// Real writes convert the entire reserved area to initialized extents. Sparse
	// truncation and fallocate alone leave allocation/conversion on the hot path.
	return zeroPostRange(ctx, file, int64(postHeaderSize), capacity-int64(postHeaderSize))
}

func zeroPostRange(ctx context.Context, file *os.File, offset, size int64) error {
	zero := make([]byte, 1<<20)
	for size > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := int64(len(zero))
		if n > size {
			n = size
		}
		written, err := file.WriteAt(zero[:n], offset)
		if err != nil {
			return err
		}
		if int64(written) != n {
			return io.ErrShortWrite
		}
		offset += n
		size -= n
	}
	return nil
}

func checkPostZeroTail(file *os.File, offset, capacity int64) error {
	buffer, zero := make([]byte, 1<<20), make([]byte, 1<<20)
	for offset < capacity {
		size := int64(len(buffer))
		if size > capacity-offset {
			size = capacity - offset
		}
		if _, err := file.ReadAt(buffer[:size], offset); err != nil {
			return err
		}
		if !bytes.Equal(buffer[:size], zero[:size]) {
			return fmt.Errorf("nonzero WAL data after end at byte %d", offset)
		}
		offset += size
	}
	return nil
}

func removeOldPostGenerations(dir, current string) error {
	paths, err := filepath.Glob(filepath.Join(dir, "*.wal"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		if filepath.Base(path) != current+".wal" {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	return nil
}

func openPostStore(ctx context.Context, db *sqlx.DB, dir string) (*postStore, error) {
	state, err := readPostState(ctx, db)
	if err != nil {
		return nil, err
	}
	if !state.Ready {
		return nil, nil
	}
	if _, err := uuid.Parse(state.Generation); err != nil {
		return nil, fmt.Errorf("invalid WAL generation: %w", err)
	}
	path := filepath.Join(dir, state.Generation+".wal")
	file, err := os.OpenFile(path, os.O_RDWR, 0640)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			file.Close()
		}
	}()
	header := make([]byte, postHeaderSize)
	if _, err := io.ReadFull(file, header); err != nil {
		return nil, err
	}
	if string(header[:postHeaderSize-8]) != postMagic+state.Generation {
		return nil, fmt.Errorf("WAL header or generation mismatch")
	}
	capacity := int64(binary.LittleEndian.Uint64(header[postHeaderSize-8:]))
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if capacity < int64(postHeaderSize+postEndSize) || capacity > postWALLimit || info.Size() != capacity {
		return nil, fmt.Errorf("WAL capacity or physical size mismatch")
	}
	s := &postStore{db: db, generation: state.Generation, file: file, capacity: capacity, streams: make(map[int64]*postStream), slots: make(chan struct{}, 256), requests: make(chan postRequest, 256), nextComment: 1, nextReaction: 1, nextWord: 1, nextReport: 1}
	if err := s.load(ctx); err != nil {
		return nil, err
	}
	var seq uint64
	offset := int64(postHeaderSize)
	for {
		if offset+postEndSize > capacity {
			return nil, fmt.Errorf("WAL has no unused end marker")
		}
		frame, size, err := readPostFrame(file, seq)
		if errors.Is(err, io.EOF) {
			if err := checkPostZeroTail(file, offset+postEndSize, capacity); err != nil {
				return nil, err
			}
			break
		}
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, errPostFrameHeader) {
			if size <= 0 || offset+size+postEndSize > capacity {
				return nil, fmt.Errorf("incomplete WAL frame exceeds available tail")
			}
			if err := checkPostZeroTail(file, offset+size, capacity); err != nil {
				return nil, err
			}
			// Erase only the incomplete frame; truncation would undo preallocation.
			if err := zeroPostRange(ctx, file, offset, size); err != nil {
				return nil, err
			}
			if err := file.Sync(); err != nil {
				return nil, err
			}
			break
		}
		if err != nil {
			return nil, fmt.Errorf("WAL at byte %d: %w", offset, err)
		}
		seq = frame[len(frame)-1].Seq
		for _, r := range frame {
			counter := s.idCounter(r.Kind)
			if counter != nil && r.ID >= *counter {
				*counter = r.ID + 1
			}
		}
		if err := s.publishRecords(frame, true); err != nil {
			return nil, err
		}
		offset += size
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	s.durableSeq.Store(seq)
	s.publishedSeq.Store(seq)
	s.durableBytes.Store(offset)
	s.publishedBytes.Store(offset)
	ok = true
	s.syncFile = func() error { return syncPostData(file) }
	s.writtenSeq, s.writtenBytes = seq, offset
	s.writers.Add(1)
	go s.writePosts()
	return s, nil
}

func encodePostFrame(records []postRecord) ([]byte, error) {
	payload, err := json.Marshal(records)
	if err != nil {
		return nil, err
	}
	if len(payload) > postFrameLimit {
		return nil, fmt.Errorf("WAL frame too large")
	}
	frame := make([]byte, postFrameHeaderSize+len(payload)+postFrameFooterSize)
	binary.LittleEndian.PutUint32(frame[:4], uint32(len(payload)))
	binary.LittleEndian.PutUint32(frame[4:8], crc32.Checksum(payload, postCRC))
	binary.LittleEndian.PutUint32(frame[8:12], ^uint32(len(payload)))
	binary.LittleEndian.PutUint32(frame[12:16], crc32.Checksum(frame[:12], postCRC))
	copy(frame[postFrameHeaderSize:], payload)
	copy(frame[postFrameHeaderSize+len(payload):], postFrameFooter)
	copy(frame[postFrameHeaderSize+len(payload)+len(postFrameFooter):], frame[:postFrameHeaderSize])
	return frame, nil
}

func readPostFrame(file io.Reader, previous uint64) ([]postRecord, int64, error) {
	var header [postFrameHeaderSize]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return nil, postFrameHeaderSize, err
	}
	if header == [postFrameHeaderSize]byte{} {
		return nil, 0, io.EOF
	}
	size := binary.LittleEndian.Uint32(header[:4])
	if size == 0 || size > postFrameLimit || binary.LittleEndian.Uint32(header[8:12]) != ^size || binary.LittleEndian.Uint32(header[12:16]) != crc32.Checksum(header[:12], postCRC) {
		// Recovery may discard this header only if everything following it is zero.
		// A damaged length cannot hide existing payloads or later complete frames.
		return nil, postFrameHeaderSize, errPostFrameHeader
	}
	payload := make([]byte, size)
	frameSize := int64(size) + int64(postFrameHeaderSize+postFrameFooterSize)
	if _, err := io.ReadFull(file, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, frameSize, err
	}
	var footer, expected, zero [postFrameFooterSize]byte
	copy(expected[:], postFrameFooter)
	copy(expected[len(postFrameFooter):], header[:])
	if _, err := io.ReadFull(file, footer[:]); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, frameSize, err
	}
	if footer != expected {
		// A prefix write into prewritten zeros leaves a missing or partial footer.
		// A complete footer requires the payload CRC to match before replay.
		for cut := 0; cut < len(footer); cut++ {
			if bytes.Equal(footer[:cut], expected[:cut]) && bytes.Equal(footer[cut:], zero[cut:]) {
				return nil, frameSize, io.ErrUnexpectedEOF
			}
		}
		return nil, 0, fmt.Errorf("WAL completion footer mismatch")
	}
	if crc32.Checksum(payload, postCRC) != binary.LittleEndian.Uint32(header[4:]) {
		return nil, 0, fmt.Errorf("WAL checksum mismatch")
	}
	var records []postRecord
	if err := json.Unmarshal(payload, &records); err != nil {
		return nil, 0, err
	}
	if len(records) == 0 || len(records) > 64 {
		return nil, 0, fmt.Errorf("invalid WAL frame count")
	}
	for _, r := range records {
		if previous == math.MaxUint64 || r.Seq != previous+1 || !validPostRecord(r) {
			return nil, 0, fmt.Errorf("invalid WAL record")
		}
		previous = r.Seq
	}
	return records, frameSize, nil
}

func writePostBytes(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

type postBatch struct {
	requests []postRequest
	records  []postRecord
	end      int64
}

func (s *postStore) writePosts() {
	defer s.writers.Done()
	s.writtenSeq = s.durableSeq.Load()
	s.writtenBytes = s.durableBytes.Load()
	s.syncWake = make(chan struct{}, 1)
	done := make(chan struct{})
	go func() { defer close(done); s.syncPosts() }()
	defer func() { close(s.syncWake); <-done }()
	for first := range s.requests {
		batch := []postRequest{first}
	collect:
		for len(batch) < 64 {
			select {
			case r, ok := <-s.requests:
				if !ok {
					break collect
				}
				batch = append(batch, r)
			default:
				break collect
			}
		}
		s.appendPostBatch(batch)
	}
}

func (s *postStore) appendPostBatch(batch []postRequest) {
	err := s.check()
	records := make([]postRecord, len(batch))
	seq := s.writtenSeq
	if err == nil {
		for i, r := range batch {
			records[i] = r.record
			counter := s.idCounter(r.record.Kind)
			if (counter != nil && *counter >= math.MaxInt64) || seq == math.MaxUint64 {
				err = fmt.Errorf("post ID or sequence exhausted")
				break
			}
			if counter != nil {
				records[i].ID = *counter
				*counter++
			}
			if records[i].Report != nil {
				copy := *records[i].Report
				copy.ID = records[i].ID
				records[i].Report = &copy
			}
			seq++
			records[i].Seq = seq
		}
	}
	var frame []byte
	if err == nil {
		frame, err = encodePostFrame(records)
	}
	if err == nil && (s.writtenBytes+int64(len(frame))+postEndSize > s.capacity || s.writtenBytes-s.durableBytes.Load()+int64(len(frame)) > postPendingLimit) {
		err = postsUnavailable("WAL capacity reached")
	}
	if err == nil {
		// Publish the frame boundary only after the complete write. Sync may overlap
		// subsequent writes, but acknowledgements never exceed its captured boundary.
		s.walMu.Lock()
		err = writePostBytes(s.file, frame)
		if err == nil {
			s.writtenBytes += int64(len(frame))
			s.writtenSeq = seq
			s.pending = append(s.pending, postBatch{requests: batch, records: records, end: s.writtenBytes})
		}
		s.walMu.Unlock()
		if err != nil {
			s.fail(fmt.Errorf("WAL write: %w", err))
		}
	}
	if err != nil {
		for i, r := range batch {
			r.done <- postResult{record: records[i], err: err}
		}
		return
	}
	select {
	case s.syncWake <- struct{}{}:
	default:
	}
}

// Capture complete frames immediately. Appends made during Sync accumulate
// for the next group and cannot be acknowledged by the current boundary.
func (s *postStore) takePostBatches() []postBatch {
	s.walMu.Lock()
	defer s.walMu.Unlock()
	batches := s.pending
	s.pending = nil
	return batches
}

func (s *postStore) syncPosts() {
	closing := false
	for {
		batches := s.takePostBatches()
		if len(batches) == 0 {
			if closing {
				return
			}
			_, ok := <-s.syncWake
			closing = !ok
			continue
		}
		s.syncPostBatches(batches)
	}
}

func (s *postStore) syncPostBatches(batches []postBatch) {
	err := s.check()
	if err == nil {
		count := 0
		for _, batch := range batches {
			count += len(batch.records)
		}
		if count == 1 {
			s.singleRecordSyncs.Add(1)
		}
		start := time.Now()
		err = s.syncFile()
		s.syncNanos.Add(uint64(time.Since(start)))
		s.syncs.Add(1)
		if err != nil {
			s.fail(fmt.Errorf("WAL Sync: %w", err))
		}
	}
	if err == nil {
		for _, batch := range batches {
			if err = s.publishRecords(batch.records, false); err != nil {
				s.fail(err)
				break
			}
		}
		if err == nil {
			last := batches[len(batches)-1]
			s.durableBytes.Store(last.end)
			s.durableSeq.Store(last.records[len(last.records)-1].Seq)
			s.publishedBytes.Store(last.end)
			s.publishedSeq.Store(s.durableSeq.Load())
			for _, batch := range batches {
				s.records.Add(uint64(len(batch.records)))
			}
		}
	}
	for _, batch := range batches {
		for i, r := range batch.requests {
			r.done <- postResult{record: batch.records[i], err: err}
		}
	}
}

func (s *postStore) idCounter(kind string) *int64 {
	switch kind {
	case "comment":
		return &s.nextComment
	case "reaction":
		return &s.nextReaction
	case "moderate":
		return &s.nextWord
	case "report":
		return &s.nextReport
	}
	return nil
}
func validPostRecord(r postRecord) bool {
	if !utf8.ValidString(r.Text) || utf8.RuneCountInString(r.Text) > 255 {
		return false
	}
	switch r.Kind {
	case "comment", "reaction", "moderate":
		if r.ID <= 0 || r.ID == math.MaxInt64 {
			return false
		}
		for _, id := range r.Deleted {
			if id <= 0 {
				return false
			}
		}
		return r.Report == nil
	case "report":
		return r.ID > 0 && r.ID < math.MaxInt64 && r.Report != nil && r.Report.ID == r.ID && r.Report.LivecommentID > 0 && r.Report.UserID == r.UserID && r.Report.LivestreamID == r.StreamID
	case "enter", "exit":
		return r.ID == 0 && r.Report == nil
	}
	return false
}
