package main

import "sort"

// Only committed SQL or published WAL references enter these indexes.
// Each request copies its pointer slice before releasing the lock.
func (s *referenceStore) indexStream(ref *livestreamReference) {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	if s.byUser == nil {
		s.byUser = make(map[int64][]*LivestreamModel)
		s.byTag = make(map[int64][]*LivestreamModel)
	}
	m := &ref.model
	s.allStreams = insertStream(s.allStreams, m, true)
	s.byUser[m.UserID] = insertStream(s.byUser[m.UserID], m, false)
	// Preserve one result per tag link, including duplicate links.
	for _, tag := range ref.tagIDs {
		s.byTag[tag] = insertStream(s.byTag[tag], m, true)
	}
}

func insertStream(rows []*LivestreamModel, model *LivestreamModel, descending bool) []*LivestreamModel {
	i := sort.Search(len(rows), func(i int) bool {
		if descending {
			return rows[i].ID <= model.ID
		}
		return rows[i].ID >= model.ID
	})
	rows = append(rows, nil)
	copy(rows[i+1:], rows[i:])
	rows[i] = model
	return rows
}

func (s *referenceStore) searchStreams(limit int, tagID int64, tagged bool) []*LivestreamModel {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	rows := s.allStreams
	if tagged {
		rows = s.byTag[tagID]
	} else if limit >= 0 && limit < len(rows) {
		rows = rows[:limit]
	}
	return append([]*LivestreamModel{}, rows...)
}

func (s *referenceStore) userStreams(userID int64) []*LivestreamModel {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	return append([]*LivestreamModel{}, s.byUser[userID]...)
}
