package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
)

type smokeWALReservation struct {
	ID           int64              `json:"id"`
	Title        string             `json:"title"`
	Description  string             `json:"description"`
	PlaylistURL  string             `json:"playlist_url"`
	ThumbnailURL string             `json:"thumbnail_url"`
	StartAt      int64              `json:"start_at"`
	EndAt        int64              `json:"end_at"`
	Owner        smokeReferenceUser `json:"owner"`
	Tags         []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"tags"`
}

func checkWALReservation(client *http.Client, write bool) error {
	const title = "Reservation WAL smoke"
	if write {
		data, err := request(client, http.MethodGet, "/api/tag", "")
		if err != nil {
			return err
		}
		var catalog struct {
			Tags []struct {
				ID int64 `json:"id"`
			} `json:"tags"`
		}
		if err := json.Unmarshal(data, &catalog); err != nil || len(catalog.Tags) < 2 {
			return fmt.Errorf("reservation tags unavailable: %v", err)
		}
		// This interval selects no SQL slot, preserving the existing zero-slot behavior.
		body, _ := json.Marshal(map[string]any{
			"title": title, "description": "WAL metadata 日本語",
			"playlist_url": "https://example.com/wal.m3u8", "thumbnail_url": "https://example.com/wal.jpg",
			"start_at": int64(1700874001), "end_at": int64(1700874002),
			"tags": []int64{catalog.Tags[1].ID, catalog.Tags[0].ID, catalog.Tags[1].ID},
		})
		res, _, err := requestResponse(client, http.MethodPost, "/api/livestream/reservation", string(body), nil)
		if err != nil || res.StatusCode != http.StatusCreated {
			return fmt.Errorf("WAL reservation failed: %v", err)
		}
	}
	data, err := request(client, http.MethodGet, "/api/livestream", "")
	if err != nil {
		return err
	}
	var streams []smokeWALReservation
	if err := json.Unmarshal(data, &streams); err != nil {
		return err
	}
	var found smokeWALReservation
	for _, stream := range streams {
		if stream.Title == title {
			found = stream
		}
	}
	if found.ID == 0 || found.Owner.Name != "test001" || found.Description != "WAL metadata 日本語" || found.PlaylistURL != "https://example.com/wal.m3u8" || found.ThumbnailURL != "https://example.com/wal.jpg" || found.StartAt != 1700874001 || found.EndAt != 1700874002 || len(found.Tags) != 3 || found.Tags[0].ID >= found.Tags[1].ID || found.Tags[1].ID != found.Tags[2].ID {
		return fmt.Errorf("reservation metadata or duplicate tags not restored")
	}
	data, err = request(client, http.MethodGet, fmt.Sprintf("/api/livestream/%d", found.ID), "")
	if err != nil {
		return err
	}
	var direct smokeWALReservation
	if err := json.Unmarshal(data, &direct); err != nil || !reflect.DeepEqual(direct, found) {
		return fmt.Errorf("reservation GET differs from owner list: %v", err)
	}
	return nil
}
