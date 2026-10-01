package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type iconEdgeStatus struct {
	PublishID  string `json:"publish_id"`
	Epoch      string `json:"epoch"`
	Revision   uint64 `json:"revision"`
	Dirty      bool   `json:"dirty"`
	ValidUntil int64  `json:"valid_until_ms"`
}

type iconEdgePublication struct {
	BasePublishID string       `json:"base_publish_id"`
	PublishID     string       `json:"publish_id"`
	Mode          string       `json:"mode"`
	Epoch         string       `json:"epoch"`
	BaseRevision  uint64       `json:"base_revision"`
	Revision      uint64       `json:"revision"`
	ValidUntil    int64        `json:"valid_until_ms"`
	Changes       []iconChange `json:"changes"`
}

// Called with the replica mutex held. The journal is indexed by origin revision.
func (s *iconReplica) appendChanges(changes []iconChange) {
	s.changes = append(s.changes, changes...)
	if len(s.changes) > 8192 {
		discard := len(s.changes) - 4096
		s.changes = append([]iconChange(nil), s.changes[discard:]...)
		s.changeBase += uint64(discard)
	}
}

func (s *iconReplica) wakeEdge() {
	select {
	case s.edgeWake <- struct{}{}:
	default:
	}
}

func (s *iconReplica) edgePublication(status iconEdgeStatus, now time.Time) iconEdgePublication {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := iconEdgePublication{BasePublishID: status.PublishID, PublishID: uuid.NewString(), Mode: "invalidate", Epoch: s.epoch, Revision: s.revision}
	if s.epoch == "" || !now.Before(s.validUntil) {
		return p
	}
	p.ValidUntil = s.validUntil.UnixMilli()
	if !status.Dirty && status.Epoch == s.epoch && status.Revision >= s.changeBase && status.Revision <= s.revision {
		p.Mode, p.BaseRevision = "delta", status.Revision
		p.Changes = append([]iconChange{}, s.changes[status.Revision-s.changeBase:]...)
	} else {
		p.Mode = "full"
		p.Changes = make([]iconChange, 0, len(s.byName))
		for name, hash := range s.byName {
			p.Changes = append(p.Changes, iconChange{Name: name, Hash: hash})
		}
	}
	return p
}

func (s *iconReplica) edgeRequest(ctx context.Context, endpoint, method string, publication *iconEdgePublication) (iconEdgeStatus, error) {
	var body io.Reader
	if publication != nil {
		data, err := json.Marshal(publication)
		if err != nil {
			return iconEdgeStatus{}, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return iconEdgeStatus{}, err
	}
	if publication != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return iconEdgeStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return iconEdgeStatus{}, fmt.Errorf("icon edge: HTTP %d", resp.StatusCode)
	}
	var status iconEdgeStatus
	err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&status)
	return status, err
}

func (s *iconReplica) runEdgePublisher(ctx context.Context, endpoint string) {
	endpoint = strings.TrimRight(endpoint, "/")
	ticker := time.NewTicker(iconSyncInterval)
	defer ticker.Stop()
	var status *iconEdgeStatus
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.edgeWake:
		case <-ticker.C:
		}
		if status == nil {
			current, err := s.edgeRequest(ctx, endpoint+"/status", http.MethodGet, nil)
			if err != nil {
				continue
			}
			status = &current
		}
		p := s.edgePublication(*status, time.Now())
		if p.Mode == "invalidate" && status.Dirty {
			continue
		}
		if p.Mode == "delta" && len(p.Changes) == 0 && p.ValidUntil == status.ValidUntil {
			continue
		}
		current, err := s.edgeRequest(ctx, endpoint+"/publish", http.MethodPost, &p)
		if err != nil {
			// Read the cursor after lost acknowledgements, restarts, or conflicts.
			status = nil
			continue
		}
		status = &current
	}
}
