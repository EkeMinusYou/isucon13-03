package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"
)

type reservationSnapshot struct {
	Model LivestreamModel `json:"model"`
	Tags  []int64         `json:"tags"`
}

var errReservationFull = errors.New("reservation slot is full")

func validReservation(r reservationSnapshot) bool {
	m := r.Model
	for _, value := range []string{m.Title, m.PlaylistUrl, m.ThumbnailUrl} {
		if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 255 {
			return false
		}
	}
	return utf8.ValidString(m.Description) && len(m.Description) <= 65535
}

// Slots are loaded from the SQL baseline once. Only WAL publication consumes them.
// The caller holds dataMu; start_at bounds preserve the SQL selection predicate.
func (s *postStore) reservationRange(m LivestreamModel) (int, int, error) {
	start := sort.Search(len(s.reservationSlots), func(i int) bool { return s.reservationSlots[i].StartAt >= m.StartAt })
	end := start
	for end < len(s.reservationSlots) && s.reservationSlots[end].StartAt < m.EndAt {
		x := s.reservationSlots[end]
		if x.EndAt <= m.EndAt && x.Slot < 1 {
			return 0, 0, errReservationFull
		}
		end++
	}
	return start, end, nil
}

func (s *postStore) reserveLivestream(ctx context.Context, refs *referenceStore, reservation *reservationSnapshot) error {
	s.life.RLock()
	defer s.life.RUnlock()
	if err := s.check(); err != nil {
		return err
	}
	select {
	case s.reservationGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.reservationGate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.dataMu.RLock()
	_, _, err := s.reservationRange(reservation.Model)
	s.dataMu.RUnlock()
	if err != nil {
		return err
	}
	if !validReservation(*reservation) {
		return fmt.Errorf("invalid livestream text")
	}
	if _, err := refs.user(ctx, reservation.Model.UserID); err != nil {
		return err
	}
	for _, id := range reservation.Tags {
		if _, ok := currentTags.Load().byID[id]; !ok {
			return sql.ErrNoRows
		}
	}
	tags := append([]int64{}, reservation.Tags...)
	sort.Slice(tags, func(i, j int) bool { return tags[i] < tags[j] })
	record := postRecord{Kind: "reservation", UserID: reservation.Model.UserID,
		Reservation: &reservationSnapshot{Model: reservation.Model, Tags: tags}}
	if err := s.reserveRow(record.cost()); err != nil {
		return err
	}
	req := postRequest{record: record, done: make(chan postResult, 1)}
	select {
	case s.requests <- req:
	case <-ctx.Done():
		s.releaseRow(record.cost())
		return ctx.Err()
	}
	// Publication must finish even if the HTTP client disconnects after acceptance.
	result := <-req.done
	if result.err != nil {
		s.releaseRow(record.cost())
		return result.err
	}
	*reservation = *result.record.Reservation
	refs.publishLivestream(*reservation)
	return nil
}

// Called under dataMu by the writer or during startup replay, before acknowledgement.
func (s *postStore) publishReservation(r postRecord, reserve bool) error {
	if r.Reservation == nil {
		return fmt.Errorf("missing reservation snapshot")
	}
	x := *r.Reservation
	if _, exists := s.owners[x.Model.ID]; exists {
		return fmt.Errorf("duplicate WAL livestream %d", x.Model.ID)
	}
	start, end, err := s.reservationRange(x.Model)
	if err != nil {
		return fmt.Errorf("WAL reservation %d: %w", x.Model.ID, err)
	}
	if reserve {
		if err := s.reserveRow(r.cost()); err != nil {
			return err
		}
	}
	if _, err := s.stream(x.Model.ID); err != nil {
		return err
	}
	for i := start; i < end; i++ {
		if s.reservationSlots[i].EndAt <= x.Model.EndAt {
			s.reservationSlots[i].Slot--
		}
	}
	s.reservations[x.Model.ID] = x
	s.addStreamLocked(x.Model.ID, x.Model.UserID)
	return nil
}
