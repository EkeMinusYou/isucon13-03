package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func admissionTicket(user, start, end int64, at time.Time) *reservationAdmission {
	return &reservationAdmission{userID: user, start: start, end: end, enqueued: at, eligibleAt: at, ready: make(chan struct{})}
}

func ticketAdmitted(ticket *reservationAdmission) bool {
	select {
	case <-ticket.ready:
		return true
	default:
		return false
	}
}

func TestReservationAdmissionPriority(t *testing.T) {
	now := time.Now()
	repeat := admissionTicket(1, 0, 3600, now.Add(-40*time.Millisecond))
	first := admissionTicket(2, 0, 3600, now.Add(-25*time.Millisecond))
	otherPeriod := admissionTicket(3, 3600, 7200, now.Add(-25*time.Millisecond))
	counts := map[int64]int{1: 4}
	s := &reservationScheduler{pending: []*reservationAdmission{repeat, first, otherPeriod}, count: func(user, _, _ int64) int { return counts[user] }}
	s.dispatchLocked(now)
	if ticketAdmitted(repeat) || !ticketAdmitted(first) || !ticketAdmitted(otherPeriod) {
		t.Fatal("first reservation must win overlapping periods while disjoint periods proceed")
	}
	// A failed transaction releases admission without changing committed counts.
	s.remove(first)
	if !ticketAdmitted(repeat) {
		t.Fatal("failed first reservation must not prevent a queued repeat reservation")
	}
	s.remove(repeat)
	s.remove(otherPeriod)
}

func TestReservationAdmissionDurationAndAging(t *testing.T) {
	now := time.Now()
	long := admissionTicket(1, 0, 19*3600, now.Add(-40*time.Millisecond))
	short := admissionTicket(2, 0, 3600, now.Add(-25*time.Millisecond))
	if !reservationBefore(short, long, 0, 0, now) {
		t.Fatal("equally new owners must prefer smaller slot consumption")
	}
	long.enqueued = now.Add(-reservationFIFOAge)
	if !reservationBefore(long, short, 5, 0, now) {
		t.Fatal("an aged repeat reservation must precede a new first reservation")
	}
	short.enqueued = long.enqueued.Add(-time.Millisecond)
	if !reservationBefore(short, long, 0, 5, now) {
		t.Fatal("aged reservations must use FIFO order")
	}
}

func TestReservationAdmissionRefreshesOwnerCount(t *testing.T) {
	now := time.Now()
	a := admissionTicket(1, 0, 3600, now.Add(-30*time.Millisecond))
	b := admissionTicket(1, 0, 3600, now.Add(-29*time.Millisecond))
	c := admissionTicket(2, 0, 3600, now.Add(-28*time.Millisecond))
	counts := map[int64]int{}
	s := &reservationScheduler{pending: []*reservationAdmission{a, b, c}, count: func(user, _, _ int64) int { return counts[user] }}
	s.dispatchLocked(now)
	if !ticketAdmitted(a) || ticketAdmitted(b) || ticketAdmitted(c) {
		t.Fatal("oldest equal-priority request must proceed first")
	}
	counts[1]++
	s.remove(a)
	if ticketAdmitted(b) || !ticketAdmitted(c) {
		t.Fatal("a second pending request must use its owner's newly committed count")
	}
	s.remove(c)
	s.remove(b)
}

func TestReservationAdmissionCancellationAndCollection(t *testing.T) {
	var reads atomic.Int64
	s := &reservationScheduler{count: func(_, _, _ int64) int { reads.Add(1); return 0 }}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		release, err := s.acquire(ctx, 1, 0, 3600)
		if release != nil {
			release()
		}
		result <- err
	}()
	// Observe the actual queued request before cancelling it.
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		queued := len(s.pending) == 1
		s.mu.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("request did not enter admission queue")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admission: %v", err)
	}
	s.mu.Lock()
	remaining := len(s.pending) + len(s.active)
	s.mu.Unlock()
	if remaining != 0 || reads.Load() != 0 {
		t.Fatal("cancelled collection must remove the request before querying priority")
	}
	release, err := s.acquire(context.Background(), 2, 0, 3600)
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending)+len(s.active) != 0 {
		t.Fatal("release must be idempotent and leave no active admissions")
	}
}

func TestReservationCountUsesCommittedOverlappingStreams(t *testing.T) {
	s := &referenceStore{}
	s.indexStream(&livestreamReference{model: LivestreamModel{ID: 1, UserID: 1, StartAt: 0, EndAt: 3600}})
	s.indexStream(&livestreamReference{model: LivestreamModel{ID: 2, UserID: 1, StartAt: 7200, EndAt: 10800}})
	if s.reservationCount(1, 3600, 7200) != 0 || s.reservationCount(1, 1800, 9000) != 2 || s.reservationCount(2, 0, 10800) != 0 {
		t.Fatal("priority must distinguish existing overlapping reservations and new owners")
	}
}
