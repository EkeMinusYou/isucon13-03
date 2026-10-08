package main

import (
	"context"
	"sync"
	"time"
)

const (
	reservationCollectWindow = 20 * time.Millisecond
	reservationFIFOAge       = 100 * time.Millisecond
	reservationParallelism   = 10
)

type reservationAdmission struct {
	userID, start, end int64
	enqueued           time.Time
	eligibleAt         time.Time
	ready              chan struct{}
}

// Admission changes only transaction order. MySQL still decides availability
// and commits reservations atomically. No transaction is held while collecting.
type reservationScheduler struct {
	mu           sync.Mutex
	pending      []*reservationAdmission
	active       []*reservationAdmission
	timer        *time.Timer
	collectUntil time.Time
	count        func(userID, start, end int64) int
}

func (s *reservationScheduler) acquire(ctx context.Context, userID, start, end int64) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ticket := &reservationAdmission{userID: userID, start: start, end: end, enqueued: time.Now(), ready: make(chan struct{})}
	s.mu.Lock()
	if !ticket.enqueued.Before(s.collectUntil) {
		s.collectUntil = ticket.enqueued.Add(reservationCollectWindow)
	}
	ticket.eligibleAt = s.collectUntil
	s.pending = append(s.pending, ticket)
	s.dispatchLocked(time.Now())
	s.mu.Unlock()
	select {
	case <-ticket.ready:
		if err := ctx.Err(); err != nil {
			s.remove(ticket)
			return nil, err
		}
		var once sync.Once
		return func() { once.Do(func() { s.remove(ticket) }) }, nil
	case <-ctx.Done():
		s.remove(ticket)
		return nil, ctx.Err()
	}
}

func removeReservationTicket(tickets []*reservationAdmission, ticket *reservationAdmission) []*reservationAdmission {
	for i, candidate := range tickets {
		if candidate == ticket {
			copy(tickets[i:], tickets[i+1:])
			tickets[len(tickets)-1] = nil
			return tickets[:len(tickets)-1]
		}
	}
	return tickets
}

func (s *reservationScheduler) remove(ticket *reservationAdmission) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = removeReservationTicket(s.pending, ticket)
	s.active = removeReservationTicket(s.active, ticket)
	s.dispatchLocked(time.Now())
}

func (s *reservationScheduler) blocked(ticket *reservationAdmission) bool {
	for _, active := range s.active {
		// Serialize one owner's concurrent requests until its committed count
		// is visible, even when the requested periods do not overlap.
		if ticket.userID == active.userID || (ticket.start < active.end && active.start < ticket.end) {
			return true
		}
	}
	return false
}

func reservationBefore(a, b *reservationAdmission, aCount, bCount int, now time.Time) bool {
	aAged := now.Sub(a.enqueued) >= reservationFIFOAge
	bAged := now.Sub(b.enqueued) >= reservationFIFOAge
	if aAged || bAged {
		if aAged != bAged {
			return aAged
		}
		return a.enqueued.Before(b.enqueued)
	}
	if (aCount == 0) != (bCount == 0) {
		return aCount == 0
	}
	// Compare durations without overflowing subtraction on malformed input.
	aDuration := uint64(a.end) - uint64(a.start)
	bDuration := uint64(b.end) - uint64(b.start)
	if aDuration != bDuration {
		return aDuration < bDuration
	}
	if aCount != bCount {
		return aCount < bCount
	}
	return a.enqueued.Before(b.enqueued)
}

func (s *reservationScheduler) dispatchLocked(now time.Time) {
	for len(s.active) < reservationParallelism {
		best, bestCount := -1, 0
		for i, ticket := range s.pending {
			if now.Before(ticket.eligibleAt) || s.blocked(ticket) {
				continue
			}
			count := s.count(ticket.userID, ticket.start, ticket.end)
			if best < 0 || reservationBefore(ticket, s.pending[best], count, bestCount, now) {
				best, bestCount = i, count
			}
		}
		if best < 0 {
			break
		}
		ticket := s.pending[best]
		s.pending = removeReservationTicket(s.pending, ticket)
		s.active = append(s.active, ticket)
		close(ticket.ready)
	}
	if s.timer != nil || len(s.active) >= reservationParallelism {
		return
	}
	var next time.Time
	for _, ticket := range s.pending {
		readyAt := ticket.eligibleAt
		if readyAt.After(now) && (next.IsZero() || readyAt.Before(next)) {
			next = readyAt
		}
	}
	if !next.IsZero() {
		s.timer = time.AfterFunc(time.Until(next), func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.timer = nil
			s.dispatchLocked(time.Now())
		})
	}
}

func (s *referenceStore) reservationCount(userID, start, end int64) int {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	count := 0
	for _, stream := range s.byUser[userID] {
		if stream.StartAt < end && start < stream.EndAt {
			count++
		}
	}
	return count
}

func (s *referenceStore) reservationScheduler() *reservationScheduler {
	s.reservationOnce.Do(func() {
		s.reservations = &reservationScheduler{count: s.reservationCount}
	})
	return s.reservations
}
