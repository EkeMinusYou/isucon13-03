package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
)

type walSmokeState struct {
	Stream    int64                      `json:"stream"`
	Removed   int64                      `json:"removed"`
	Responses map[string]json.RawMessage `json:"responses"`
}

func checkMutationPersistence(client *http.Client, write bool) error {
	if err := checkPostPersistence(client, write); err != nil {
		return err
	}
	path := filepath.Join(os.TempDir(), "isucon-smoke", "wal-primary-state.json")
	var state walSmokeState
	if write {
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
			data, err := request(client, http.MethodGet, fmt.Sprintf("/api/livestream/%d/ngwords", stream.ID), "")
			if err != nil {
				return err
			}
			var words []json.RawMessage
			if err := json.Unmarshal(data, &words); err != nil {
				return err
			}
			if len(words) == 0 {
				state.Stream = stream.ID
				break
			}
		}
		if state.Stream == 0 {
			return fmt.Errorf("no unmoderated owned stream for WAL check")
		}
		base := fmt.Sprintf("/api/livestream/%d", state.Stream)
		res, body, err := requestResponse(client, http.MethodPost, base+"/livecomment", `{"comment":"wal_primary_removed","tip":11}`, nil)
		if err != nil {
			return err
		}
		var comment struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(body, &comment); err != nil || res.StatusCode != 201 || comment.ID <= 0 {
			return fmt.Errorf("WAL comment: status=%d body=%s", res.StatusCode, body)
		}
		state.Removed = comment.ID
		for _, op := range []struct {
			method, path, body string
			status             int
		}{
			{http.MethodPost, fmt.Sprintf("%s/livecomment/%d/report", base, comment.ID), "", 201},
			{http.MethodPost, base + "/moderate", `{"ng_word":"wal_primary_removed"}`, 201},
			{http.MethodPost, base + "/livecomment", `{"comment":"WAL_PRIMARY_REMOVED","tip":11}`, 400},
			{http.MethodPost, base + "/enter", "", 200},
			{http.MethodPost, base + "/enter", "", 200},
			{http.MethodDelete, base + "/exit", "", 200},
			{http.MethodPost, base + "/enter", "", 200},
		} {
			res, body, err := requestResponse(client, op.method, op.path, op.body, nil)
			if err != nil {
				return err
			}
			if res.StatusCode != op.status {
				return fmt.Errorf("WAL operation %s %s: status=%d want=%d body=%s", op.method, op.path, res.StatusCode, op.status, body)
			}
		}
		state.Responses, err = walSmokeResponses(client, state)
		if err != nil {
			return err
		}
		data, err = json.Marshal(state)
		if err != nil {
			return err
		}
		return os.WriteFile(path, data, 0600)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	got, err := walSmokeResponses(client, state)
	if err != nil {
		return err
	}
	for path, want := range state.Responses {
		var before, after any
		if err := json.Unmarshal(want, &before); err != nil {
			return err
		}
		if err := json.Unmarshal(got[path], &after); err != nil {
			return err
		}
		if !reflect.DeepEqual(before, after) {
			return fmt.Errorf("WAL recovery changed %s: before=%s after=%s", path, want, got[path])
		}
	}
	return nil
}

func walSmokeResponses(client *http.Client, state walSmokeState) (map[string]json.RawMessage, error) {
	base := fmt.Sprintf("/api/livestream/%d", state.Stream)
	result := make(map[string]json.RawMessage)
	for _, path := range []string{base + "/livecomment", base + "/reaction", base + "/ngwords", base + "/report", base + "/statistics", "/api/user/test001/statistics", "/api/payment"} {
		data, err := request(client, http.MethodGet, path, "")
		if err != nil {
			return nil, err
		}
		if !json.Valid(data) {
			return nil, fmt.Errorf("invalid WAL response: %s", path)
		}
		result[path] = data
	}
	var comments []struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(result[base+"/livecomment"], &comments); err != nil {
		return nil, err
	}
	for _, comment := range comments {
		if comment.ID == state.Removed {
			return nil, fmt.Errorf("moderated comment resurrected")
		}
	}
	var reports []struct {
		Livecomment struct {
			ID      int64  `json:"id"`
			Comment string `json:"comment"`
		} `json:"livecomment"`
	}
	if err := json.Unmarshal(result[base+"/report"], &reports); err != nil {
		return nil, err
	}
	found := false
	for _, report := range reports {
		if report.Livecomment.ID == state.Removed && report.Livecomment.Comment == "wal_primary_removed" {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("report snapshot of deleted comment lost")
	}
	return result, nil
}
