package main

import (
	"context"
	"database/sql"
	"fmt"
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

// A builder reuses reference values within one response; timelines remain in MySQL.
type responseBuilder struct {
	ctx         context.Context
	refs        *referenceStore
	users       map[int64]User
	livestreams map[int64]Livestream
	tags        *tagCatalog
}

func newResponseBuilder(ctx context.Context) *responseBuilder {
	return &responseBuilder{
		ctx: ctx, refs: currentReferences.Load(), users: make(map[int64]User),
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
	if b.refs == nil {
		return fmt.Errorf("reference store is not loaded")
	}
	for _, id := range uniqueIDs(ids) {
		if _, ok := b.users[id]; ok {
			continue
		}
		user, err := b.refs.user(b.ctx, id)
		if err != nil {
			return err
		}
		b.users[id] = user
	}
	return nil
}

func (b *responseBuilder) loadLivestreams(models []*LivestreamModel, userIDs []int64) error {
	for _, model := range models {
		userIDs = append(userIDs, model.UserID)
	}
	if err := b.loadUsers(userIDs); err != nil {
		return err
	}
	if len(models) == 0 {
		return nil
	}
	if b.tags == nil {
		return fmt.Errorf("tag catalog is not loaded")
	}
	for _, model := range models {
		ref, err := b.refs.livestream(b.ctx, model.ID)
		if err != nil {
			return err
		}
		tags := make([]Tag, 0, len(ref.tagIDs))
		for _, id := range ref.tagIDs {
			tag, ok := b.tags.byID[id]
			if !ok {
				return sql.ErrNoRows
			}
			tags = append(tags, tag)
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
	if b.refs == nil {
		return fmt.Errorf("reference store is not loaded")
	}
	models := make([]*LivestreamModel, 0, len(ids))
	for _, id := range ids {
		ref, err := b.refs.livestream(b.ctx, id)
		if err != nil {
			return err
		}
		models = append(models, &ref.model)
	}
	return b.loadLivestreams(models, userIDs)
}

func fillLivestreamResponses(ctx context.Context, tx *sqlx.Tx, models []*LivestreamModel) ([]Livestream, error) {
	b := newResponseBuilder(ctx)
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
	b := newResponseBuilder(ctx)
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
	b := newResponseBuilder(ctx)
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
	b := newResponseBuilder(ctx)
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
