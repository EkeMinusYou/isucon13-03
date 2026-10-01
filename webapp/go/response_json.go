package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/labstack/echo/v4"
)

// Keep one immutable response per reference. Icon hashes and the tag catalog
// identify the mutable dependencies; readers never edit published byte slices.
type referenceJSON struct {
	hash string
	tags *tagCatalog
	data []byte
	user *User
}

func (s *referenceStore) userJSON(ctx context.Context, id int64) (*referenceJSON, error) {
	value, ok := s.users.Load(id)
	if !ok {
		return nil, sql.ErrNoRows
	}
	ref := value.(*userReference)
	if err := ref.wait(ctx); err != nil {
		return nil, err
	}
	hash := ref.icon.hash.Load().(string)
	if cached := ref.response.Load(); cached != nil && cached.hash == hash {
		return cached, nil
	}
	user := ref.user
	user.IconHash = hash
	data, err := json.Marshal(user)
	if err != nil {
		return nil, err
	}
	cached := &referenceJSON{hash: hash, data: data, user: &user}
	ref.response.Store(cached)
	return cached, nil
}

func (ref *livestreamReference) responseJSON(catalog *tagCatalog, owner User) ([]byte, error) {
	if catalog == nil {
		return nil, fmt.Errorf("tag catalog is not loaded")
	}
	if cached := ref.response.Load(); cached != nil && cached.hash == owner.IconHash && cached.tags == catalog {
		return cached.data, nil
	}
	tags := make([]Tag, 0, len(ref.tagIDs))
	for _, id := range ref.tagIDs {
		tag, ok := catalog.byID[id]
		if !ok {
			return nil, sql.ErrNoRows
		}
		tags = append(tags, tag)
	}
	m := ref.model
	data, err := json.Marshal(Livestream{
		ID: m.ID, Owner: owner, Title: m.Title, Description: m.Description,
		PlaylistUrl: m.PlaylistUrl, ThumbnailUrl: m.ThumbnailUrl, Tags: tags,
		StartAt: m.StartAt, EndAt: m.EndAt,
	})
	if err != nil {
		return nil, err
	}
	ref.response.Store(&referenceJSON{hash: owner.IconHash, tags: catalog, data: data})
	return data, nil
}

type responseJSONBuffer struct{ data []byte }

var responseJSONBuffers = sync.Pool{New: func() any { return &responseJSONBuffer{} }}

func acquireResponseJSON() *responseJSONBuffer {
	b := responseJSONBuffers.Get().(*responseJSONBuffer)
	b.data = append(b.data[:0], '[')
	return b
}

func releaseResponseJSON(b *responseJSONBuffer) {
	// Unbounded searches must not make every pooled scratch buffer retain megabytes.
	if cap(b.data) <= 128<<10 {
		b.data = b.data[:0]
		responseJSONBuffers.Put(b)
	}
}

func writeResponseJSON(c echo.Context, b *responseJSONBuffer) error {
	if _, pretty := c.QueryParams()["pretty"]; pretty || c.Echo().Debug {
		var indented bytes.Buffer
		if err := json.Indent(&indented, b.data, "", "  "); err != nil {
			return err
		}
		indented.WriteByte('\n')
		return c.JSONBlob(http.StatusOK, indented.Bytes())
	}
	b.data = append(b.data, '\n')
	return c.JSONBlob(http.StatusOK, b.data)
}

type responseJSONBuilder struct {
	ctx     context.Context
	refs    *referenceStore
	tags    *tagCatalog
	users   map[int64]*referenceJSON
	streams map[int64][]byte
}

func newResponseJSONBuilder(ctx context.Context) *responseJSONBuilder {
	return &responseJSONBuilder{ctx: ctx, refs: currentReferences.Load(), tags: currentTags.Load(),
		users: make(map[int64]*referenceJSON), streams: make(map[int64][]byte)}
}

func (b *responseJSONBuilder) user(id int64) (*referenceJSON, error) {
	if data, ok := b.users[id]; ok {
		return data, nil
	}
	if b.refs == nil {
		return nil, fmt.Errorf("reference store is not loaded")
	}
	data, err := b.refs.userJSON(b.ctx, id)
	if err == nil {
		b.users[id] = data
	}
	return data, err
}

func (b *responseJSONBuilder) stream(id int64) ([]byte, error) {
	if data, ok := b.streams[id]; ok {
		return data, nil
	}
	if b.refs == nil {
		return nil, fmt.Errorf("reference store is not loaded")
	}
	ref, err := b.refs.livestream(b.ctx, id)
	if err != nil {
		return nil, err
	}
	owner, err := b.user(ref.model.UserID)
	if err != nil {
		return nil, err
	}
	data, err := ref.responseJSON(b.tags, *owner.user)
	if err == nil {
		b.streams[id] = data
	}
	return data, err
}

func appendResponseString(dst []byte, value string) []byte {
	// Encoding a string cannot fail; use the standard encoder for escaping,
	// invalid UTF-8 replacement, HTML characters and Unicode separators.
	encoded, _ := json.Marshal(value)
	return append(dst, encoded...)
}

func livestreamResponseJSON(ctx context.Context, models []*LivestreamModel) (*responseJSONBuffer, error) {
	b := acquireResponseJSON()
	refs := newResponseJSONBuilder(ctx)
	for i, model := range models {
		data, err := refs.stream(model.ID)
		if err != nil {
			releaseResponseJSON(b)
			return nil, err
		}
		if i > 0 {
			b.data = append(b.data, ',')
		}
		b.data = append(b.data, data...)
	}
	b.data = append(b.data, ']')
	return b, nil
}

func livecommentResponseJSON(ctx context.Context, models []LivecommentModel) (*responseJSONBuffer, error) {
	b := acquireResponseJSON()
	refs := newResponseJSONBuilder(ctx)
	for i, model := range models {
		user, err := refs.user(model.UserID)
		if err != nil {
			releaseResponseJSON(b)
			return nil, err
		}
		stream, err := refs.stream(model.LivestreamID)
		if err != nil {
			releaseResponseJSON(b)
			return nil, err
		}
		if i > 0 {
			b.data = append(b.data, ',')
		}
		b.data = append(b.data, `{"id":`...)
		b.data = strconv.AppendInt(b.data, model.ID, 10)
		b.data = append(b.data, `,"user":`...)
		b.data = append(b.data, user.data...)
		b.data = append(b.data, `,"livestream":`...)
		b.data = append(b.data, stream...)
		b.data = append(b.data, `,"comment":`...)
		b.data = appendResponseString(b.data, model.Comment)
		b.data = append(b.data, `,"tip":`...)
		b.data = strconv.AppendInt(b.data, model.Tip, 10)
		b.data = append(b.data, `,"created_at":`...)
		b.data = strconv.AppendInt(b.data, model.CreatedAt, 10)
		b.data = append(b.data, '}')
	}
	b.data = append(b.data, ']')
	return b, nil
}

func reactionResponseJSON(ctx context.Context, models []ReactionModel) (*responseJSONBuffer, error) {
	b := acquireResponseJSON()
	refs := newResponseJSONBuilder(ctx)
	for i, model := range models {
		user, err := refs.user(model.UserID)
		if err != nil {
			releaseResponseJSON(b)
			return nil, err
		}
		stream, err := refs.stream(model.LivestreamID)
		if err != nil {
			releaseResponseJSON(b)
			return nil, err
		}
		if i > 0 {
			b.data = append(b.data, ',')
		}
		b.data = append(b.data, `{"id":`...)
		b.data = strconv.AppendInt(b.data, model.ID, 10)
		b.data = append(b.data, `,"emoji_name":`...)
		b.data = appendResponseString(b.data, model.EmojiName)
		b.data = append(b.data, `,"user":`...)
		b.data = append(b.data, user.data...)
		b.data = append(b.data, `,"livestream":`...)
		b.data = append(b.data, stream...)
		b.data = append(b.data, `,"created_at":`...)
		b.data = strconv.AppendInt(b.data, model.CreatedAt, 10)
		b.data = append(b.data, '}')
	}
	b.data = append(b.data, ']')
	return b, nil
}
