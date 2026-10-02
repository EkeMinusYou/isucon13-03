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

const postMagic = "ISUPWAL4"
const postHeaderSize = len(postMagic) + 36 + 8
const postFrameLimit = 1 << 20
const postFrameHeaderSize = 16
const postFrameFooter = "WALDONE4"
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
	IconHash  string                  `json:"hash,omitempty"`
	IconTotal int64                   `json:"total,omitempty"`
	IconStart int64                   `json:"start,omitempty"`
	IconSize  int                     `json:"size,omitempty"`
	iconData  []byte
	iconPos   int64
}

func (r postRecord) cost() int64 {
	switch r.Kind {
	case "moderate":
		// Include the per-author cached JSON, whose escaping can expand a byte
		// to six bytes, as well as the stored NG word and map entry.
		return int64(512 + 7*len(r.Text))
	case "exit":
		return 0
	case "icon":
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
	s := &postStore{db: db, generation: state.Generation, file: file, capacity: capacity, streams: make(map[int64]*postStream), slots: make(chan struct{}, 256), requests: make(chan postRequest, 256), iconRequests: make(chan *iconWrite, 16), icons: make(map[string]*walIcon), nextComment: 1, nextReaction: 1, nextWord: 1, nextReport: 1}
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
		for i := range frame {
			frame[i].iconPos += offset
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
	// Complete frames at the tail may contain an unfinished image. Such an image
	// has no published DB pointer and is deliberately absent from the read index.
	s.discardIncompleteIcons()
	ok = true
	s.syncFile = func() error { return syncPostData(file) }
	s.writtenSeq, s.writtenBytes = seq, offset
	s.writers.Add(1)
	go s.writePosts()
	return s, nil
}

func encodePostFrame(records []postRecord) ([]byte, error) {
	meta, err := json.Marshal(records)
	if err != nil {
		return nil, err
	}
	size := 4 + len(meta)
	for _, r := range records {
		if len(r.iconData) != r.IconSize {
			return nil, fmt.Errorf("WAL icon size mismatch")
		}
		size += len(r.iconData)
	}
	payload := make([]byte, size)
	binary.LittleEndian.PutUint32(payload[:4], uint32(len(meta)))
	copy(payload[4:], meta)
	offset := 4 + len(meta)
	for i := range records {
		if records[i].Kind == "icon" {
			records[i].iconPos = int64(postFrameHeaderSize + offset)
		}
		copy(payload[offset:], records[i].iconData)
		offset += len(records[i].iconData)
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
	if len(payload) < 4 {
		return nil, 0, fmt.Errorf("WAL metadata prefix missing")
	}
	metaSize := int(binary.LittleEndian.Uint32(payload[:4]))
	if metaSize > len(payload)-4 {
		return nil, 0, fmt.Errorf("WAL metadata size mismatch")
	}
	if err := json.Unmarshal(payload[4:4+metaSize], &records); err != nil {
		return nil, 0, err
	}
	if len(records) == 0 || len(records) > 64 {
		return nil, 0, fmt.Errorf("invalid WAL frame count")
	}
	dataOffset := 4 + metaSize
	for i := range records {
		r := &records[i]
		if r.IconSize < 0 || r.IconSize > len(payload)-dataOffset {
			return nil, 0, fmt.Errorf("WAL icon payload size mismatch")
		}
		if r.Kind == "icon" {
			r.iconData = payload[dataOffset : dataOffset+r.IconSize]
			r.iconPos = int64(postFrameHeaderSize + dataOffset)
		}
		dataOffset += r.IconSize
		if previous == math.MaxUint64 || r.Seq != previous+1 || !validPostRecord(*r) {
			return nil, 0, fmt.Errorf("invalid WAL record")
		}
		previous = r.Seq
	}
	if dataOffset != len(payload) {
		return nil, 0, fmt.Errorf("WAL has unused payload bytes")
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
	queuedAt time.Time
}

const postSyncCollectDelay = 200 * time.Microsecond
const postSyncCollectRecords = 8

func (s *postStore) writePosts() {
	defer s.writers.Done()
	s.writtenSeq = s.durableSeq.Load()
	s.writtenBytes = s.durableBytes.Load()
	s.syncWake = make(chan struct{}, 1)
	done := make(chan struct{})
	go func() { defer close(done); s.syncPosts() }()
	defer func() { close(s.syncWake); <-done }()
	posts, icons := s.requests, s.iconRequests
	var active []*iconWrite
	for posts != nil || icons != nil || len(active) > 0 {
		batch := make([]postRequest, 0, 64)
		// Rotate image jobs in 64 KiB units. Every round also drains ready posts.
		// The sync worker bounds collection by the oldest frame's age.
		if len(active) == 0 && len(posts) == 0 {
			select {
			case r, ok := <-posts:
				if !ok {
					posts = nil
				} else {
					batch = append(batch, r)
				}
			case job, ok := <-icons:
				if !ok {
					icons = nil
				} else {
					active = append(active, job)
				}
			}
		}
		if icons != nil {
			select {
			case job, ok := <-icons:
				if !ok {
					icons = nil
				} else {
					active = append(active, job)
				}
			default:
			}
		}
	collect:
		for len(batch) < 63 && posts != nil {
			select {
			case r, ok := <-posts:
				if !ok {
					posts = nil
					break collect
				}
				batch = append(batch, r)
			default:
				break collect
			}
		}
		if len(active) > 0 {
			job := active[0]
			active = active[1:]
			if err := s.check(); err != nil {
				job.done <- postResult{err: err}
			} else {
				n := len(job.data) - job.offset
				if n > iconChunkSize {
					n = iconChunkSize
				}
				r := postRequest{record: postRecord{Kind: "icon", IconHash: job.hash, IconTotal: int64(len(job.data)), IconStart: int64(job.offset), IconSize: n, iconData: job.data[job.offset : job.offset+n]}}
				job.offset += n
				if job.offset == len(job.data) {
					r.done = job.done
				} else {
					active = append(active, job)
				}
				batch = append(batch, r)
			}
		}
		if len(batch) > 0 {
			s.appendPostBatch(batch)
		}
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
		for i := range records {
			records[i].iconPos += s.writtenBytes
		}
		err = writePostBytes(s.file, frame)
		if err == nil {
			s.writtenBytes += int64(len(frame))
			s.writtenSeq = seq
			s.pending = append(s.pending, postBatch{requests: batch, records: records, end: s.writtenBytes, queuedAt: time.Now()})
		}
		s.walMu.Unlock()
		if err != nil {
			s.fail(fmt.Errorf("WAL write: %w", err))
		}
	}
	if err != nil {
		// Never continue a partially accepted image after a capacity/encoding error.
		s.fail(err)
		for i, r := range batch {
			if r.done != nil {
				r.done <- postResult{record: records[i], err: err}
			}
		}
		return
	}
	select {
	case s.syncWake <- struct{}{}:
	default:
	}
}

// Capture only complete frames. Appends made during Sync accumulate for the
// next group and cannot be acknowledged by the current boundary.
func (s *postStore) takePostBatches() []postBatch {
	s.walMu.Lock()
	defer s.walMu.Unlock()
	batches := s.pending
	s.pending = nil
	return batches
}

func (s *postStore) collectPostBatches(batches []postBatch) ([]postBatch, bool) {
	count := 0
	for _, batch := range batches {
		count += len(batch.records)
	}
	// An isolated request needs no added wait. A backlog older than the budget
	// also syncs immediately, so repeated notifications cannot extend its age.
	wait := time.Until(batches[0].queuedAt.Add(postSyncCollectDelay))
	if count >= postSyncCollectRecords || len(s.slots) <= 1 || wait <= 0 {
		return batches, false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for count < postSyncCollectRecords {
		select {
		case _, ok := <-s.syncWake:
			more := s.takePostBatches()
			for _, batch := range more {
				count += len(batch.records)
			}
			batches = append(batches, more...)
			if !ok {
				return batches, true
			}
		case <-timer.C:
			return append(batches, s.takePostBatches()...), false
		}
	}
	return batches, false
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
		if !closing {
			batches, closing = s.collectPostBatches(batches)
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
				for _, r := range batch.records {
					if r.Kind == "icon" {
						s.iconChunks.Add(1)
						s.iconBytes.Add(uint64(r.IconSize))
					} else {
						s.records.Add(1)
					}
				}
			}
		}
	}
	for _, batch := range batches {
		for i, r := range batch.requests {
			if r.done != nil {
				r.done <- postResult{record: batch.records[i], err: err}
			}
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
	if r.Kind == "icon" {
		return r.ID == 0 && r.Report == nil && validIconHash(r.IconHash) && r.IconTotal >= 0 && r.IconTotal <= iconMaxSize && r.IconStart >= 0 && r.IconStart <= r.IconTotal && r.IconSize >= 0 && r.IconSize <= iconChunkSize && r.IconStart+int64(r.IconSize) <= r.IconTotal && (r.IconSize > 0 || r.IconTotal == 0)
	}
	if r.IconSize != 0 || r.IconHash != "" || r.IconTotal != 0 || r.IconStart != 0 {
		return false
	}
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
