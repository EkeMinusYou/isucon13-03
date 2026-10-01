package main

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jmoiron/sqlx"
)

// Only readers of a newly inserted ID wait for its commit and publication.
// Existing references never hold a global lock while MySQL commits.
type referencePublication struct {
	ready chan struct{}
	err   error
}

func (p *referencePublication) wait(ctx context.Context) error {
	if p.ready == nil {
		return nil
	}
	select {
	case <-p.ready:
		return p.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type userReference struct {
	referencePublication
	user     User
	icon     *iconEntry
	response atomic.Pointer[referenceJSON]
}

type livestreamReference struct {
	referencePublication
	model    LivestreamModel
	tagIDs   []int64
	response atomic.Pointer[referenceJSON]
}

type referenceStore struct {
	users       sync.Map // int64 -> *userReference
	livestreams sync.Map // int64 -> *livestreamReference
}

var currentReferences atomic.Pointer[referenceStore]

func reloadReferences(ctx context.Context) error {
	tx, err := dbConn.BeginTxx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var users []struct {
		ID          int64  `db:"id"`
		Name        string `db:"name"`
		DisplayName string `db:"display_name"`
		Description string `db:"description"`
		ThemeID     int64  `db:"theme_id"`
		DarkMode    bool   `db:"dark_mode"`
	}
	if err := tx.SelectContext(ctx, &users, `
	SELECT u.id, u.name, u.display_name, u.description,
	       t.id AS theme_id, t.dark_mode
	FROM users u INNER JOIN themes t ON t.user_id = u.id
	ORDER BY u.id, t.id`); err != nil {
		return err
	}
	store := &referenceStore{}
	currentIcons.mu.RLock()
	for _, row := range users {
		if _, exists := store.users.Load(row.ID); exists {
			continue
		}
		icon := currentIcons.byID[row.ID]
		if icon == nil {
			currentIcons.mu.RUnlock()
			return fmt.Errorf("missing icon reference for user %d", row.ID)
		}
		store.users.Store(row.ID, &userReference{user: User{
			ID: row.ID, Name: row.Name, DisplayName: row.DisplayName, Description: row.Description,
			Theme: Theme{ID: row.ThemeID, DarkMode: row.DarkMode},
		}, icon: icon})
	}
	currentIcons.mu.RUnlock()
	var streams []LivestreamModel
	if err := tx.SelectContext(ctx, &streams, "SELECT * FROM livestreams"); err != nil {
		return err
	}
	for _, model := range streams {
		store.livestreams.Store(model.ID, &livestreamReference{model: model, tagIDs: []int64{}})
	}
	var links []LivestreamTagModel
	if err := tx.SelectContext(ctx, &links, "SELECT * FROM livestream_tags ORDER BY livestream_id, tag_id, id"); err != nil {
		return err
	}
	for _, link := range links {
		if value, ok := store.livestreams.Load(link.LivestreamID); ok {
			ref := value.(*livestreamReference)
			ref.tagIDs = append(ref.tagIDs, link.TagID)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	currentReferences.Store(store)
	return nil
}

func (s *referenceStore) user(ctx context.Context, id int64) (User, error) {
	value, ok := s.users.Load(id)
	if !ok {
		return User{}, sql.ErrNoRows
	}
	ref := value.(*userReference)
	if err := ref.wait(ctx); err != nil {
		return User{}, err
	}
	user := ref.user
	// Share the icon store's atomic pointer so nested responses see every upload.
	user.IconHash = ref.icon.hash.Load().(string)
	return user, nil
}

func (s *referenceStore) namedUser(ctx context.Context, name string) (User, error) {
	currentIcons.mu.RLock()
	entry := currentIcons.byName[strings.TrimRight(name, " ")]
	currentIcons.mu.RUnlock()
	if entry == nil {
		return User{}, sql.ErrNoRows
	}
	return s.user(ctx, entry.id)
}

func (s *referenceStore) livestream(ctx context.Context, id int64) (*livestreamReference, error) {
	value, ok := s.livestreams.Load(id)
	if !ok {
		return nil, sql.ErrNoRows
	}
	ref := value.(*livestreamReference)
	if err := ref.wait(ctx); err != nil {
		return nil, err
	}
	return ref, nil
}

func (s *referenceStore) commitUser(tx *sqlx.Tx, user User) error {
	ref := &userReference{user: user, referencePublication: referencePublication{ready: make(chan struct{})}}
	s.users.Store(user.ID, ref)
	if err := tx.Commit(); err != nil {
		s.users.Delete(user.ID)
		ref.err = sql.ErrNoRows
		close(ref.ready)
		return err
	}
	ref.icon = currentIcons.addUser(user.ID, user.Name, user.IconHash)
	if store := currentMutations.Load(); store != nil {
		store.addUser(user.ID, user.Name)
	}
	close(ref.ready)
	return nil
}

func (s *referenceStore) commitLivestream(ctx context.Context, tx *sqlx.Tx, model LivestreamModel, tagIDs []int64) error {
	if _, err := s.user(ctx, model.UserID); err != nil {
		return err
	}
	catalog := currentTags.Load()
	for _, id := range tagIDs {
		if _, ok := catalog.byID[id]; !ok {
			return sql.ErrNoRows
		}
	}
	tags := append([]int64{}, tagIDs...)
	sort.Slice(tags, func(i, j int) bool { return tags[i] < tags[j] })
	ref := &livestreamReference{model: model, tagIDs: tags, referencePublication: referencePublication{ready: make(chan struct{})}}
	s.livestreams.Store(model.ID, ref)
	if err := tx.Commit(); err != nil {
		s.livestreams.Delete(model.ID)
		ref.err = sql.ErrNoRows
		close(ref.ready)
		return err
	}
	if err := ensurePostStream(model.ID, model.UserID); err != nil {
		ref.err = err
		close(ref.ready)
		return err
	}
	close(ref.ready)
	return nil
}
