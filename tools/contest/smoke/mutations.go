package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

func checkMutationPersistence(client *http.Client, write bool) error {
	const marker = "bulk-writer-smoke"
	const emoji = "bulk_writer_smoke"
	data, err := request(client, http.MethodGet, "/api/livestream", "")
	if err != nil {
		return err
	}
	var streams []struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(data, &streams); err != nil {
		return err
	}
	for _, stream := range streams {
		base := fmt.Sprintf("/api/livestream/%d", stream.ID)
		data, err := request(client, http.MethodGet, base+"/ngwords", "")
		if err != nil {
			return err
		}
		var words []json.RawMessage
		if err := json.Unmarshal(data, &words); err != nil {
			return err
		}
		if len(words) != 0 {
			continue
		}
		var commentID, reactionID int64
		if write {
			for _, post := range []struct {
				path, body string
				id         *int64
			}{
				{base + "/livecomment", `{"comment":"bulk-writer-smoke","tip":7}`, &commentID},
				{base + "/reaction", `{"emoji_name":"bulk_writer_smoke"}`, &reactionID},
			} {
				res, body, err := requestResponse(client, http.MethodPost, post.path, post.body, nil)
				if err != nil {
					return err
				}
				var result struct {
					ID int64 `json:"id"`
				}
				if err := json.Unmarshal(body, &result); err != nil || res.StatusCode != http.StatusCreated || result.ID <= 0 {
					return fmt.Errorf("mutation POST %s: status=%d, decode=%v", post.path, res.StatusCode, err)
				}
				*post.id = result.ID
			}
		}
		data, err = request(client, http.MethodGet, base+"/livecomment", "")
		if err != nil {
			return err
		}
		var comments []struct {
			ID      int64  `json:"id"`
			Comment string `json:"comment"`
			Tip     int64  `json:"tip"`
			User    struct {
				Name string `json:"name"`
			} `json:"user"`
			Livestream struct {
				ID int64 `json:"id"`
			} `json:"livestream"`
		}
		if err := json.Unmarshal(data, &comments); err != nil {
			return err
		}
		foundComment := false
		for _, c := range comments {
			if c.ID > 0 && c.Comment == marker && c.Tip == 7 && c.User.Name == "test001" && c.Livestream.ID == stream.ID && (commentID == 0 || c.ID == commentID) {
				foundComment = true
			}
		}
		data, err = request(client, http.MethodGet, base+"/reaction", "")
		if err != nil {
			return err
		}
		var reactions []struct {
			ID    int64  `json:"id"`
			Emoji string `json:"emoji_name"`
			User  struct {
				Name string `json:"name"`
			} `json:"user"`
			Livestream struct {
				ID int64 `json:"id"`
			} `json:"livestream"`
		}
		if err := json.Unmarshal(data, &reactions); err != nil {
			return err
		}
		foundReaction := false
		for _, r := range reactions {
			if r.ID > 0 && r.Emoji == emoji && r.User.Name == "test001" && r.Livestream.ID == stream.ID && (reactionID == 0 || r.ID == reactionID) {
				foundReaction = true
			}
		}
		if foundComment && foundReaction {
			return nil
		}
		if write {
			return fmt.Errorf("acknowledged mutation not readable")
		}
	}
	return fmt.Errorf("no durable mutation smoke pair on an unmoderated owned stream")
}
