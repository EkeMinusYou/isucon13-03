package main

import (
	"bytes"
	"context"
	"reflect"
	"testing"
)

func TestStreamIndexesPreserveOrderAndDuplicates(t *testing.T) {
	s := &referenceStore{}
	// Reservations can commit in a different order from their generated IDs.
	for _, id := range []int64{4, 1, 3, 2} {
		s.indexStream(&livestreamReference{model: LivestreamModel{ID: id, UserID: 1}, tagIDs: []int64{7, 7}})
	}
	ids := func(rows []*LivestreamModel) []int64 {
		out := make([]int64, len(rows))
		for i, row := range rows {
			out[i] = row.ID
		}
		return out
	}
	for _, tc := range []struct {
		rows []*LivestreamModel
		want []int64
	}{
		{s.searchStreams(2, 0, false), []int64{4, 3}},
		{s.searchStreams(1, 7, true), []int64{4, 4, 3, 3, 2, 2, 1, 1}},
		{s.userStreams(1), []int64{1, 2, 3, 4}},
		{s.searchStreams(0, 0, false), []int64{}},
	} {
		if got := ids(tc.rows); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("ids=%v want=%v", got, tc.want)
		}
	}
	snapshot := s.searchStreams(-1, 0, false)
	s.indexStream(&livestreamReference{model: LivestreamModel{ID: 5, UserID: 1}})
	if !reflect.DeepEqual(ids(snapshot), []int64{4, 3, 2, 1}) {
		t.Fatal("index update modified a request snapshot")
	}
}

func TestRowJSONCacheReusesRowsAndRefreshesDependencies(t *testing.T) {
	_, _, user := responseJSONFixture(t)
	ctx := context.Background()
	model := LivecommentModel{ID: 1, UserID: 1, LivestreamID: 2, Comment: "<cached>", Tip: 7}
	one, err := newResponseJSONBuilder(ctx).commentJSON(model)
	if err != nil {
		t.Fatal(err)
	}
	two, err := newResponseJSONBuilder(ctx).commentJSON(model)
	if err != nil || &one[0] != &two[0] {
		t.Fatal("unchanged row was not reused", err)
	}
	before := append([]byte(nil), one...)
	user.icon.hash.Store("changed")
	three, err := newResponseJSONBuilder(ctx).commentJSON(model)
	if err != nil || bytes.Equal(three, one) || !bytes.Equal(before, one) {
		t.Fatal("icon refresh or immutable response failed", err)
	}
	model.Tip++
	four, err := newResponseJSONBuilder(ctx).commentJSON(model)
	if err != nil || bytes.Equal(four, three) {
		t.Fatal("changed row reused an old payload", err)
	}
}

func TestBoundedCacheEvictionAndOversizedEntries(t *testing.T) {
	var cache boundedCache[string, []byte]
	cache.put("one", []byte("one"), 8, 10)
	retained, _ := cache.get("one")
	cache.put("two", []byte("two"), 8, 10)
	if _, ok := cache.get("one"); ok {
		t.Fatal("full cache retained an evicted key")
	}
	cache.put("large", []byte("large"), 11, 10)
	if _, ok := cache.get("large"); ok {
		t.Fatal("oversized entry was retained")
	}
	if string(retained) != "one" {
		t.Fatal("eviction modified a reader's bytes")
	}
	budget := cache.bytes
	if budget > 10 {
		t.Fatalf("cache exceeded its budget: %d", budget)
	}
}
