package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/labstack/echo/v4"
)

type iconEntry struct {
	name string
	id   int64
	mu   sync.Mutex
	hash atomic.Value
}

type iconStore struct {
	dir          string
	fallbackHash string
	lifecycle    sync.RWMutex
	mu           sync.RWMutex
	byName       map[string]*iconEntry
	byID         map[int64]*iconEntry
	epoch        string
	revision     uint64
	changeBase   uint64
	changes      []iconChange
}

var currentIcons *iconStore

func iconDirectory() string {
	if dir := os.Getenv("ISUCON_ICON_DIR"); dir != "" {
		return dir
	}
	return "/var/lib/isupipe/icons"
}

func newIconStore(ctx context.Context, dir string) (*iconStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	s := &iconStore{dir: dir}
	image, err := os.ReadFile(fallbackImage)
	if err != nil {
		return nil, err
	}
	s.fallbackHash, err = s.persist(image)
	if err != nil {
		return nil, err
	}
	if err := s.load(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *iconStore) syncDirectory() error {
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// A database pointer is published only after the file and its directory are durable.
func (s *iconStore) persist(image []byte) (string, error) {
	digest := sha256.Sum256(image)
	hash := hex.EncodeToString(digest[:])
	path := filepath.Join(s.dir, hash)
	if _, err := os.Stat(path); err == nil {
		// Another upload may have just renamed an already-synced identical file.
		return hash, s.syncDirectory()
	} else if !os.IsNotExist(err) {
		return "", err
	}
	file, err := os.CreateTemp(s.dir, ".upload-")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0644); err != nil {
		return "", err
	}
	if _, err := file.Write(image); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return "", err
	}
	return hash, s.syncDirectory()
}

func validIconHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, c := range hash {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *iconStore) load(ctx context.Context) error {
	var users []struct {
		ID   int64  `db:"id"`
		Name string `db:"name"`
		Hash string `db:"icon_hash"`
	}
	if err := dbConn.SelectContext(ctx, &users, "SELECT id, name, icon_hash FROM users"); err != nil {
		return err
	}
	byName := make(map[string]*iconEntry, len(users))
	byID := make(map[int64]*iconEntry, len(users))
	checked := make(map[string]bool)
	for _, user := range users {
		hash := user.Hash
		if hash == "" {
			hash = s.fallbackHash
		}
		if !validIconHash(hash) {
			return fmt.Errorf("invalid icon hash for user %d", user.ID)
		}
		if !checked[hash] {
			if _, err := os.Stat(filepath.Join(s.dir, hash)); err != nil {
				return fmt.Errorf("missing icon for user %d: %w", user.ID, err)
			}
			checked[hash] = true
		}
		entry := &iconEntry{id: user.ID, name: strings.TrimRight(user.Name, " ")}
		entry.hash.Store(hash)
		// The users' utf8mb4_bin collation ignores trailing ASCII spaces.
		byName[strings.TrimRight(user.Name, " ")] = entry
		byID[user.ID] = entry
	}
	s.mu.Lock()
	s.byName, s.byID = byName, byID
	s.epoch = uuid.NewString()
	s.revision, s.changeBase, s.changes = 0, 0, nil
	s.mu.Unlock()
	return nil
}

func (s *iconStore) addUser(id int64, name, hash string) *iconEntry {
	entry := &iconEntry{id: id, name: strings.TrimRight(name, " ")}
	entry.hash.Store(hash)
	s.mu.Lock()
	s.byName[strings.TrimRight(name, " ")] = entry
	s.byID[id] = entry
	s.changedIcon(entry.name, hash)
	update := iconUpdate{Epoch: s.epoch, Revision: s.revision, Name: entry.name, Hash: hash}
	s.mu.Unlock()
	notifyIconReplica(update)
	return entry
}

// The caller holds lifecycle exclusively after the official database initialization.
func (s *iconStore) reset(ctx context.Context) error {
	files, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, file := range files {
		if file.Name() != s.fallbackHash && (validIconHash(file.Name()) || strings.HasPrefix(file.Name(), ".upload-")) {
			if err := os.Remove(filepath.Join(s.dir, file.Name())); err != nil {
				return err
			}
		}
	}
	if err := s.syncDirectory(); err != nil {
		return err
	}
	return s.load(ctx)
}

func iconETagMatches(header, hash string) bool {
	for _, value := range strings.Split(header, ",") {
		value = strings.TrimPrefix(strings.TrimSpace(value), "W/")
		if value == "\""+hash+"\"" {
			return true
		}
	}
	return false
}

func getIconHandler(c echo.Context) error {
	s := currentIcons
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	s.mu.RLock()
	entry := s.byName[strings.TrimRight(c.Param("username"), " ")]
	s.mu.RUnlock()
	if entry == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found user that has the given username")
	}
	hash := entry.hash.Load().(string)
	c.Response().Header().Set("ETag", "\""+hash+"\"")
	if iconETagMatches(c.Request().Header.Get("If-None-Match"), hash) {
		return c.NoContent(http.StatusNotModified)
	}
	// Keep immutable image files with their owner when nginx runs on another host.
	file, err := os.Open(filepath.Join(s.dir, hash))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to open icon: "+err.Error())
	}
	defer file.Close()
	return c.Stream(http.StatusOK, "image/jpeg", file)
}

func postIconHandler(c echo.Context) error {
	if err := verifyUserSession(c); err != nil {
		return err
	}
	userID := sessionUserID(c)
	var req PostIconRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to decode the request body as json")
	}
	s := currentIcons
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	s.mu.RLock()
	entry := s.byID[userID]
	s.mu.RUnlock()
	if entry == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found user that has the userid in session")
	}
	hash, err := s.persist(req.Image)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to persist icon: "+err.Error())
	}
	// Keep each user's commit and publication in the same order.
	entry.mu.Lock()
	defer entry.mu.Unlock()
	ctx := c.Request().Context()
	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "INSERT INTO icons (user_id, hash) VALUES (?, ?)", userID, hash)
	if err != nil {
		return err
	}
	iconID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET icon_hash = ? WHERE id = ?", hash, userID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	entry.hash.Store(hash)
	s.changedIcon(entry.name, hash)
	update := iconUpdate{Epoch: s.epoch, Revision: s.revision, Name: entry.name, Hash: hash}
	s.mu.Unlock()
	notifyIconReplica(update)
	return c.JSON(http.StatusCreated, PostIconResponse{ID: iconID})
}
