package main

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"
)

func TestReservationAcceptedCancellationAndFrame(t *testing.T) {
	refs, _, _ := responseJSONFixture(t)
	s := writerTestStore(t)
	s.nextStream = 3
	s.reservationGate = make(chan struct{}, 1)
	s.reservations = make(map[int64]reservationSnapshot)
	s.reservationSlots = []ReservationSlotModel{{ID: 1, Slot: 1, StartAt: 10, EndAt: 20}}
	s.addUser(1, "owner")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reservation := reservationSnapshot{Model: LivestreamModel{UserID: 1, Title: "日本語", Description: "snapshot", StartAt: 10, EndAt: 20}, Tags: []int64{2, 1, 2}}
	done := make(chan error, 1)
	go func() { done <- s.reserveLivestream(ctx, refs, &reservation) }()
	var req postRequest
	select {
	case req = <-s.requests:
	case <-time.After(time.Second):
		t.Fatal("reservation was not accepted")
	}
	cancel()
	s.writtenBytes = int64(postHeaderSize)
	s.appendPostBatch([]postRequest{req})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted reservation was abandoned after cancellation")
	}
	if reservation.Model.ID != 3 || s.reservationSlots[0].Slot != 0 || !reflect.DeepEqual(reservation.Tags, []int64{1, 2, 2}) {
		t.Fatalf("published reservation: %+v slots=%v", reservation, s.reservationSlots)
	}
	if ref, err := refs.livestream(context.Background(), 3); err != nil || !reflect.DeepEqual(ref.model, reservation.Model) {
		t.Fatalf("accepted reference missing: %v %v", ref, err)
	}
	frame := make([]byte, s.writtenBytes-int64(postHeaderSize))
	if _, err := s.file.ReadAt(frame, int64(postHeaderSize)); err != nil {
		t.Fatal(err)
	}
	records, _, err := readPostFrame(bytes.NewReader(frame), 0)
	if err != nil || len(records) != 1 || !reflect.DeepEqual(*records[0].Reservation, reservation) {
		t.Fatalf("reservation frame: %+v err=%v", records, err)
	}
	bad := records[0]
	bad.StreamID++
	if validPostRecord(bad) {
		t.Fatal("inconsistent reservation ID accepted")
	}
}

func TestReservationCanceledBeforeAcceptance(t *testing.T) {
	refs, _, _ := responseJSONFixture(t)
	s := writerTestStore(t)
	s.reservationGate = make(chan struct{}, 1)
	s.reservationGate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.reserveLivestream(ctx, refs, &reservationSnapshot{}); err != context.Canceled || len(s.requests) != 0 || s.rows != 0 {
		t.Fatalf("canceled reservation accepted: err=%v pending=%d rows=%d", err, len(s.requests), s.rows)
	}
}
