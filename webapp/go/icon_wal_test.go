package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSharedWALImagePublicationReuseAndPosts(t *testing.T) {
	s := writerTestStore(t)
	started, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(release) }) })
	var calls atomic.Int64
	s.syncFile = func() error {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return s.file.Sync()
	}
	s.writers.Add(1)
	go s.writePosts()
	image := bytes.Repeat([]byte("fragmented-image"), 10000)
	digest := sha256.Sum256(image)
	key := hex.EncodeToString(digest[:])
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, duplicate, post := make(chan error, 1), make(chan error, 1), make(chan error, 1)
	go func() { _, err := s.persistIcon(ctx, image); first <- err }()
	<-started
	cancel()
	go func() { _, err := s.persistIcon(context.Background(), image); duplicate <- err }()
	model := &ReactionModel{LivestreamID: 1, EmojiName: "alongside-image"}
	go func() { post <- s.submit(context.Background(), 1, &reactionMutation{model: model}) }()
	for _, done := range []chan error{first, duplicate, post} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("image or post completion waited for Sync")
		}
	}
	if s.durableSeq.Load() != 0 || s.publishedSeq.Load() != 4 {
		t.Fatal("image and post publication was not independent of Sync")
	}
	reader, err := s.iconReader(key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, image) {
		t.Fatalf("fragment read mismatch: %v", err)
	}
	before := s.iconChunks.Load()
	if _, err := s.persistIcon(context.Background(), image); err != nil || s.iconChunks.Load() != before {
		t.Fatal("complete image was written again")
	}
	_, reactions, err := s.timeline(context.Background(), 1, -1, false)
	if err != nil || len(reactions) != 1 || reactions[0] != *model {
		t.Fatalf("mixed post missing: %+v %v", reactions, err)
	}
	if s.iconUploads.Load() != 1 || s.iconChunks.Load() != 3 {
		t.Fatal("duplicate upload created extra image records")
	}
	unblock.Do(func() { close(release) })
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	if s.durableSeq.Load() != s.publishedSeq.Load() {
		t.Fatal("close did not drain image and post writes")
	}
}

func TestSharedWALMixedFrameAndImageHash(t *testing.T) {
	image := []byte{0, 255, 0, 128, 34, 92}
	digest := sha256.Sum256(image)
	key := hex.EncodeToString(digest[:])
	frame, err := encodePostFrame([]postRecord{
		{Seq: 1, Kind: "reaction", ID: 1, StreamID: 1, Text: "mixed"},
		{Seq: 2, Kind: "icon", IconHash: key, IconTotal: int64(len(image)), IconSize: len(image), iconData: image},
	})
	if err != nil {
		t.Fatal(err)
	}
	records, _, err := readPostFrame(bytes.NewReader(frame), 0)
	if err != nil || len(records) != 2 || !bytes.Equal(records[1].iconData, image) {
		t.Fatalf("binary image frame: %v", err)
	}
	if !bytes.Equal(frame[records[1].iconPos:records[1].iconPos+int64(len(image))], image) {
		t.Fatal("image position is incorrect")
	}
	s := &postStore{}
	if err := s.publishIconChunk(records[1]); err != nil || !s.hasIcon(key) {
		t.Fatalf("image hash verification: %v", err)
	}
	bad := records[1]
	bad.IconHash = hex.EncodeToString(make([]byte, 32))
	if err := s.publishIconChunk(bad); err == nil {
		t.Fatal("incorrect image hash was accepted")
	}
	// A retry replaces its old incomplete entry without consuming another slot.
	s = &postStore{icons: make(map[string]*walIcon)}
	partial := records[1]
	partial.IconSize = 1
	partial.iconData = image[:1]
	if err := s.publishIconChunk(partial); err != nil {
		t.Fatal(err)
	}
	for i := 0; len(s.icons) < iconIndexLimit; i++ {
		s.icons[fmt.Sprintf("%064x", i)] = &walIcon{}
	}
	if err := s.publishIconChunk(records[1]); err != nil || !s.hasIcon(key) {
		t.Fatalf("retry at index capacity: %v", err)
	}
}

func TestSharedWALImageWriteFailure(t *testing.T) {
	s := writerTestStore(t)
	if err := s.file.Close(); err != nil {
		t.Fatal(err)
	}
	s.writers.Add(1)
	go s.writePosts()
	image := []byte("never-written")
	key := sha256.Sum256(image)
	if _, err := s.persistIcon(context.Background(), image); err == nil {
		t.Fatal("failed image write acknowledged")
	}
	if s.hasIcon(hex.EncodeToString(key[:])) {
		t.Fatal("failed image write published blob")
	}
}

func TestSharedWALImageAsyncSyncFailure(t *testing.T) {
	s := writerTestStore(t)
	started, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(release) }) })
	s.syncFile = func() error {
		close(started)
		<-release
		return errors.New("injected image Sync failure")
	}
	s.writers.Add(1)
	go s.writePosts()
	image := []byte("written-before-sync-failure")
	key, err := s.persistIcon(context.Background(), image)
	if err != nil || !s.hasIcon(key) {
		t.Fatalf("written image was not published: %v", err)
	}
	<-started
	unblock.Do(func() { close(release) })
	if err := s.close(); err == nil || s.durableSeq.Load() != 0 {
		t.Fatal("async image sync failure was not reported")
	}
	if _, err := s.persistIcon(context.Background(), image); err == nil {
		t.Fatal("failed engine reused an image")
	}
}
