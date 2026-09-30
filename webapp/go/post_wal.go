package main

import (
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

const postMagic = "ISUPWAL1"
const postHeaderSize = len(postMagic) + 36
const postFrameLimit = 1 << 20

var postCRC = crc32.MakeTable(crc32.Castagnoli)

type postRecord struct {
	Seq       uint64 `json:"seq"`
	Kind      string `json:"kind"`
	ID        int64  `json:"id"`
	UserID    int64  `json:"user"`
	StreamID  int64  `json:"stream"`
	Text      string `json:"text"`
	Tip       int64  `json:"tip,omitempty"`
	CreatedAt int64  `json:"created"`
}

func (r postRecord) cost() int64 { return int64(256 + len(r.Text)) }
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
	Applied    uint64 `db:"applied_seq"`
}

func readPostState(ctx context.Context, db *sqlx.DB) (postDBState, error) {
	var state postDBState
	err := db.GetContext(ctx, &state, "SELECT generation,ready,applied_seq FROM post_wal_state WHERE id=1")
	return state, err
}

func createPostGeneration(ctx context.Context, db *sqlx.DB, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	gen := uuid.NewString()
	file, err := os.OpenFile(filepath.Join(dir, gen+".wal"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
	if err != nil {
		return "", err
	}
	err = writePostBytes(file, []byte(postMagic+gen))
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
	result, err := db.ExecContext(ctx, "UPDATE post_wal_state SET generation=?,ready=TRUE,applied_seq=0 WHERE id=1 AND ready=FALSE", gen)
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
	if string(header) != postMagic+state.Generation {
		return nil, fmt.Errorf("WAL header or generation mismatch")
	}
	s := &postStore{db: db, generation: state.Generation, file: file, changed: make(chan struct{}), streams: make(map[int64]*postStream), slots: make(chan struct{}, 256), requests: make(chan postRequest, 256), wake: make(chan struct{}, 1), stop: make(chan struct{}), nextComment: 1, nextReaction: 1}
	var seq uint64
	offset := int64(postHeaderSize)
	markerFound := state.Applied == 0
	for {
		frame, size, err := readPostFrame(file, seq)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			if err := file.Truncate(offset); err != nil {
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
			if r.Kind == "comment" && r.ID >= s.nextComment {
				s.nextComment = r.ID + 1
			}
			if r.Kind == "reaction" && r.ID >= s.nextReaction {
				s.nextReaction = r.ID + 1
			}
		}
		if seq == state.Applied {
			markerFound = true
		}
		if seq > state.Applied {
			if !markerFound {
				return nil, fmt.Errorf("DB marker is not a WAL frame boundary")
			}
			if err := s.projectFrameBatch(ctx, frame); err != nil {
				return nil, err
			}
		}
		offset += size
	}
	if !markerFound || seq < state.Applied {
		return nil, fmt.Errorf("DB marker exceeds available WAL")
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	s.reader, err = os.Open(path)
	if err != nil {
		return nil, err
	}
	if _, err := s.reader.Seek(offset, io.SeekStart); err != nil {
		s.reader.Close()
		return nil, err
	}
	s.durableSeq.Store(seq)
	s.appliedSeq.Store(seq)
	s.durableBytes.Store(offset)
	s.appliedBytes.Store(offset)
	if err := s.load(ctx); err != nil {
		s.reader.Close()
		return nil, err
	}
	ok = true
	s.syncFile = file.Sync
	s.writers.Add(1)
	go s.writePosts()
	s.projectors.Add(1)
	go s.projectPosts()
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
	frame := make([]byte, 8+len(payload))
	binary.LittleEndian.PutUint32(frame[:4], uint32(len(payload)))
	binary.LittleEndian.PutUint32(frame[4:8], crc32.Checksum(payload, postCRC))
	copy(frame[8:], payload)
	return frame, nil
}

func readPostFrame(file io.Reader, previous uint64) ([]postRecord, int64, error) {
	var header [8]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return nil, 0, err
	}
	size := binary.LittleEndian.Uint32(header[:4])
	if size == 0 || size > postFrameLimit {
		return nil, 0, fmt.Errorf("invalid WAL frame size")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(file, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, 0, err
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
		if previous == math.MaxUint64 || r.Seq != previous+1 || r.ID <= 0 || r.ID == math.MaxInt64 || (r.Kind != "comment" && r.Kind != "reaction") || !utf8.ValidString(r.Text) || utf8.RuneCountInString(r.Text) > 255 {
			return nil, 0, fmt.Errorf("invalid WAL record")
		}
		previous = r.Seq
	}
	return records, int64(size) + 8, nil
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

func (s *postStore) writePosts() {
	defer s.writers.Done()
	for first := range s.requests {
		batch := []postRequest{first}
		timer := time.NewTimer(time.Millisecond)
	collect:
		for len(batch) < 64 {
			select {
			case r, ok := <-s.requests:
				if !ok {
					break collect
				}
				batch = append(batch, r)
			case <-timer.C:
				break collect
			}
		}
		timer.Stop()
		s.writePostBatch(batch)
	}
}

func (s *postStore) writePostBatch(batch []postRequest) {
	err := s.check()
	records := make([]postRecord, len(batch))
	seq := s.durableSeq.Load()
	if err == nil {
		for i, r := range batch {
			records[i] = r.record
			counter := &s.nextReaction
			if r.record.Kind == "comment" {
				counter = &s.nextComment
			}
			if *counter >= math.MaxInt64 || seq == math.MaxUint64 {
				err = fmt.Errorf("post ID or sequence exhausted")
				break
			}
			records[i].ID = *counter
			*counter++
			seq++
			records[i].Seq = seq
		}
	}
	var frame []byte
	if err == nil {
		frame, err = encodePostFrame(records)
	}
	if err == nil && (s.durableBytes.Load()+int64(len(frame)) > postWALLimit || s.durableBytes.Load()-s.appliedBytes.Load()+int64(len(frame)) > postPendingLimit) {
		err = postsUnavailable("WAL capacity reached")
	}
	if err == nil {
		start := time.Now()
		err = writePostBytes(s.file, frame)
		if err == nil {
			err = s.syncFile()
		}
		s.syncNanos.Add(uint64(time.Since(start)))
		s.syncs.Add(1)
		if err != nil {
			s.fail(fmt.Errorf("WAL write or Sync: %w", err))
		} else {
			s.mu.Lock()
			s.durableBytes.Add(int64(len(frame)))
			s.durableSeq.Store(seq)
			s.records.Add(uint64(len(records)))
			s.mu.Unlock()
		}
	}
	for i, r := range batch {
		r.done <- postResult{record: records[i], err: err}
	}
}
