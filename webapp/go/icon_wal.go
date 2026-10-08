package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
)

const iconChunkSize = 64 << 10
const iconMaxSize = 10 << 20
const iconIndexLimit = 32768
const iconUploadLimit = 16 << 20

type iconPart struct {
	offset int64
	size   int64
}

type walIcon struct {
	total    int64
	written  int64
	parts    []iconPart
	digest   hash.Hash
	complete bool
	ready    chan struct{}
	err      error
}

type iconWrite struct {
	hash   string
	data   []byte
	offset int
	done   chan postResult
}

func (s *postStore) persistIcon(ctx context.Context, image []byte) (string, error) {
	s.life.RLock()
	defer s.life.RUnlock()
	if err := s.check(); err != nil {
		return "", err
	}
	if len(image) > iconMaxSize {
		return "", fmt.Errorf("icon exceeds size limit")
	}
	digest := sha256.Sum256(image)
	key := hex.EncodeToString(digest[:])
	s.iconMu.Lock()
	if entry := s.icons[key]; entry != nil {
		s.iconReuses.Add(1)
		s.iconMu.Unlock()
		// Share completion of the entire file write before publishing this hash
		// in MySQL; fdatasync runs independently of upload completion.
		select {
		case <-entry.ready:
			return key, entry.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if len(s.icons) >= iconIndexLimit || s.iconPendingBytes+int64(len(image)) > iconUploadLimit || s.iconRequests == nil {
		s.iconMu.Unlock()
		return "", postsUnavailable("icon WAL admission limit reached")
	}
	if s.icons == nil {
		s.icons = make(map[string]*walIcon)
	}
	entry := &walIcon{total: int64(len(image)), digest: sha256.New(), ready: make(chan struct{})}
	s.icons[key] = entry
	s.iconPendingBytes += int64(len(image))
	s.iconMu.Unlock()
	job := &iconWrite{hash: key, data: image, done: make(chan postResult, 1)}
	var err error
	select {
	case s.iconRequests <- job:
		// Accepted data remains owned by the request until its last written chunk.
		err = (<-job.done).err
	case <-ctx.Done():
		err = ctx.Err()
	}
	s.iconMu.Lock()
	s.iconPendingBytes -= int64(len(image))
	entry.err = err
	if err != nil {
		delete(s.icons, key)
	} else {
		s.iconUploads.Add(1)
	}
	close(entry.ready)
	s.iconMu.Unlock()
	if err != nil {
		return "", err
	}
	return key, nil
}

func (s *postStore) publishIconChunk(r postRecord) error {
	s.iconMu.Lock()
	defer s.iconMu.Unlock()
	if s.icons == nil {
		s.icons = make(map[string]*walIcon)
	}
	entry := s.icons[r.IconHash]
	if entry != nil && !entry.complete && entry.written > 0 && r.IconStart == 0 {
		entry = nil
	}
	if entry == nil {
		if r.IconStart != 0 || (s.icons[r.IconHash] == nil && len(s.icons) >= iconIndexLimit) {
			return fmt.Errorf("invalid icon WAL start or index limit")
		}
		entry = &walIcon{total: r.IconTotal, digest: sha256.New(), ready: make(chan struct{})}
		s.icons[r.IconHash] = entry
	}
	if entry.complete || entry.total != r.IconTotal || entry.written != r.IconStart || len(r.iconData) != r.IconSize {
		return fmt.Errorf("invalid icon WAL chunk ordering")
	}
	if _, err := entry.digest.Write(r.iconData); err != nil {
		return err
	}
	entry.parts = append(entry.parts, iconPart{offset: r.iconPos, size: int64(r.IconSize)})
	entry.written += int64(r.IconSize)
	if entry.written == entry.total {
		if hex.EncodeToString(entry.digest.Sum(nil)) != r.IconHash {
			return fmt.Errorf("icon WAL hash mismatch")
		}
		entry.complete = true
		entry.digest = nil
	}
	return nil
}

func (s *postStore) discardIncompleteIcons() {
	for key, entry := range s.icons {
		if !entry.complete {
			delete(s.icons, key)
		} else {
			close(entry.ready)
		}
	}
}

func (s *postStore) iconReader(key string) (io.Reader, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	s.iconMu.Lock()
	entry := s.icons[key]
	if entry == nil || !entry.complete {
		s.iconMu.Unlock()
		return nil, fmt.Errorf("missing complete icon %s", key)
	}
	parts := entry.parts
	s.iconMu.Unlock()
	readers := make([]io.Reader, len(parts))
	for i, p := range parts {
		readers[i] = io.NewSectionReader(s.file, p.offset, p.size)
	}
	return io.MultiReader(readers...), nil
}

func (s *postStore) hasIcon(key string) bool {
	s.iconMu.Lock()
	defer s.iconMu.Unlock()
	return s.icons[key] != nil && s.icons[key].complete
}
