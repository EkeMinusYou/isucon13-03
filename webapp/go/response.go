package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/jmoiron/sqlx"
)

type tagCatalog struct {
	tags []Tag
	byID map[int64]Tag
}

var currentTags atomic.Pointer[tagCatalog]

func reloadTags(ctx context.Context) error {
	var models []TagModel
	if err := dbConn.SelectContext(ctx, &models, "SELECT * FROM tags ORDER BY id"); err != nil {
		return err
	}
	catalog := &tagCatalog{tags: make([]Tag, len(models)), byID: make(map[int64]Tag, len(models))}
	for i, model := range models {
		tag := Tag{ID: model.ID, Name: model.Name}
		catalog.tags[i] = tag
		catalog.byID[tag.ID] = tag
	}
	currentTags.Store(catalog)
	return nil
}

// A builder belongs to one transaction, so mutable data never outlives its snapshot.
type responseBuilder struct {
	ctx         context.Context
	tx          *sqlx.Tx
	users       map[int64]User
	livestreams map[int64]Livestream
	tags        *tagCatalog
}

func newResponseBuilder(ctx context.Context, tx *sqlx.Tx) *responseBuilder {
	return &responseBuilder{
		ctx: ctx, tx: tx, users: make(map[int64]User),
		livestreams: make(map[int64]Livestream), tags: currentTags.Load(),
	}
}

func uniqueIDs(ids []int64) []int64 {
	seen := make(map[int64]bool, len(ids))
	result := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	return result
}

func (b *responseBuilder) loadUsers(ids []int64) error {
	missing := make([]int64, 0, len(ids))
	for _, id := range uniqueIDs(ids) {
		if _, ok := b.users[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	query, args, err := sqlx.In(`
	SELECT u.id, u.name, u.display_name, u.description,
	       t.id AS theme_id, t.dark_mode, i.id AS icon_id, i.image
	FROM users u
	INNER JOIN themes t ON t.user_id = u.id
	LEFT JOIN icons i ON i.user_id = u.id
	WHERE u.id IN (?)
	ORDER BY u.id, t.id, i.id`, missing)
	if err != nil {
		return err
	}
	var rows []struct {
		ID          int64         `db:"id"`
		Name        string        `db:"name"`
		DisplayName string        `db:"display_name"`
		Description string        `db:"description"`
		ThemeID     int64         `db:"theme_id"`
		DarkMode    bool          `db:"dark_mode"`
		IconID      sql.NullInt64 `db:"icon_id"`
		Image       []byte        `db:"image"`
	}
	if err := b.tx.SelectContext(b.ctx, &rows, query, args...); err != nil {
		return err
	}
	var fallback []byte
	for _, row := range rows {
		if _, ok := b.users[row.ID]; ok {
			continue
		}
		image := row.Image
		if !row.IconID.Valid {
			if fallback == nil {
				fallback, err = os.ReadFile(fallbackImage)
				if err != nil {
					return err
				}
			}
			image = fallback
		}
		hash := sha256.Sum256(image)
		b.users[row.ID] = User{
			ID: row.ID, Name: row.Name, DisplayName: row.DisplayName, Description: row.Description,
			Theme: Theme{ID: row.ThemeID, DarkMode: row.DarkMode}, IconHash: fmt.Sprintf("%x", hash),
		}
	}
	for _, id := range missing {
		if _, ok := b.users[id]; !ok {
			return sql.ErrNoRows
		}
	}
	return nil
}

func (b *responseBuilder) loadLivestreams(models []*LivestreamModel, userIDs []int64) error {
	ids := make([]int64, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
		userIDs = append(userIDs, model.UserID)
	}
	if err := b.loadUsers(userIDs); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	if b.tags == nil {
		return fmt.Errorf("tag catalog is not loaded")
	}
	query, args, err := sqlx.In("SELECT * FROM livestream_tags WHERE livestream_id IN (?) ORDER BY livestream_id, tag_id, id", uniqueIDs(ids))
	if err != nil {
		return err
	}
	var links []LivestreamTagModel
	if err := b.tx.SelectContext(b.ctx, &links, query, args...); err != nil {
		return err
	}
	byStream := make(map[int64][]Tag, len(models))
	for _, link := range links {
		tag, ok := b.tags.byID[link.TagID]
		if !ok {
			return sql.ErrNoRows
		}
		byStream[link.LivestreamID] = append(byStream[link.LivestreamID], tag)
	}
	for _, model := range models {
		tags := byStream[model.ID]
		if tags == nil {
			tags = []Tag{}
		}
		b.livestreams[model.ID] = Livestream{
			ID: model.ID, Owner: b.users[model.UserID], Title: model.Title, Description: model.Description,
			PlaylistUrl: model.PlaylistUrl, ThumbnailUrl: model.ThumbnailUrl, Tags: tags,
			StartAt: model.StartAt, EndAt: model.EndAt,
		}
	}
	return nil
}

func (b *responseBuilder) loadLivestreamIDs(ids, userIDs []int64) error {
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return b.loadUsers(userIDs)
	}
	query, args, err := sqlx.In("SELECT * FROM livestreams WHERE id IN (?)", ids)
	if err != nil {
		return err
	}
	var models []*LivestreamModel
	if err := b.tx.SelectContext(b.ctx, &models, query, args...); err != nil {
		return err
	}
	if len(models) != len(ids) {
		return sql.ErrNoRows
	}
	return b.loadLivestreams(models, userIDs)
}

func fillLivestreamResponses(ctx context.Context, tx *sqlx.Tx, models []*LivestreamModel) ([]Livestream, error) {
	b := newResponseBuilder(ctx, tx)
	if err := b.loadLivestreams(models, nil); err != nil {
		return nil, err
	}
	result := make([]Livestream, len(models))
	for i, model := range models {
		result[i] = b.livestreams[model.ID]
	}
	return result, nil
}

func fillLivecommentResponses(ctx context.Context, tx *sqlx.Tx, models []LivecommentModel) ([]Livecomment, error) {
	b := newResponseBuilder(ctx, tx)
	ids, userIDs := make([]int64, len(models)), make([]int64, len(models))
	for i, model := range models {
		ids[i], userIDs[i] = model.LivestreamID, model.UserID
	}
	if err := b.loadLivestreamIDs(ids, userIDs); err != nil {
		return nil, err
	}
	result := make([]Livecomment, len(models))
	for i, model := range models {
		result[i] = b.livecomment(model)
	}
	return result, nil
}

func (b *responseBuilder) livecomment(model LivecommentModel) Livecomment {
	return Livecomment{
		ID: model.ID, User: b.users[model.UserID], Livestream: b.livestreams[model.LivestreamID],
		Comment: model.Comment, Tip: model.Tip, CreatedAt: model.CreatedAt,
	}
}

func fillReactionResponses(ctx context.Context, tx *sqlx.Tx, models []ReactionModel) ([]Reaction, error) {
	b := newResponseBuilder(ctx, tx)
	ids, userIDs := make([]int64, len(models)), make([]int64, len(models))
	for i, model := range models {
		ids[i], userIDs[i] = model.LivestreamID, model.UserID
	}
	if err := b.loadLivestreamIDs(ids, userIDs); err != nil {
		return nil, err
	}
	result := make([]Reaction, len(models))
	for i, model := range models {
		result[i] = Reaction{
			ID: model.ID, EmojiName: model.EmojiName, User: b.users[model.UserID],
			Livestream: b.livestreams[model.LivestreamID], CreatedAt: model.CreatedAt,
		}
	}
	return result, nil
}

func fillLivecommentReportResponses(ctx context.Context, tx *sqlx.Tx, models []*LivecommentReportModel) ([]LivecommentReport, error) {
	result := make([]LivecommentReport, len(models))
	if len(models) == 0 {
		return result, nil
	}
	commentIDs := make([]int64, len(models))
	userIDs := make([]int64, 0, len(models)*2)
	for i, model := range models {
		commentIDs[i] = model.LivecommentID
		userIDs = append(userIDs, model.UserID)
	}
	query, args, err := sqlx.In("SELECT * FROM livecomments WHERE id IN (?)", uniqueIDs(commentIDs))
	if err != nil {
		return nil, err
	}
	var comments []LivecommentModel
	if err := tx.SelectContext(ctx, &comments, query, args...); err != nil {
		return nil, err
	}
	byID := make(map[int64]LivecommentModel, len(comments))
	ids := make([]int64, 0, len(comments))
	for _, comment := range comments {
		byID[comment.ID] = comment
		ids = append(ids, comment.LivestreamID)
		userIDs = append(userIDs, comment.UserID)
	}
	b := newResponseBuilder(ctx, tx)
	if err := b.loadLivestreamIDs(ids, userIDs); err != nil {
		return nil, err
	}
	for i, model := range models {
		comment, ok := byID[model.LivecommentID]
		if !ok {
			return nil, sql.ErrNoRows
		}
		result[i] = LivecommentReport{
			ID: model.ID, Reporter: b.users[model.UserID], Livecomment: b.livecomment(comment), CreatedAt: model.CreatedAt,
		}
	}
	return result, nil
}
