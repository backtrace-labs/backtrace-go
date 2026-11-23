package bt

import (
	"sync"
	"time"
)

const rateWindow = time.Minute

type reportLimiter struct {
	mu      sync.Mutex
	limit   int
	ring    []time.Time
	head    int
	count   int
	limited bool
	now     func() time.Time
}

func newReportLimiter(perMinute int) *reportLimiter {
	if perMinute <= 0 {
		return nil
	}
	return &reportLimiter{limit: perMinute, now: time.Now}
}

func (l *reportLimiter) allow() (slot time.Time, ok, first bool) {
	if l == nil {
		return time.Time{}, true, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Read the clock under the lock. Expiry assumes the ring is in time order.
	now := l.now()
	for l.count > 0 && now.Sub(l.ring[l.head]) >= rateWindow {
		l.head = (l.head + 1) % len(l.ring)
		l.count--
	}
	if l.count >= l.limit {
		first = !l.limited
		l.limited = true
		return time.Time{}, false, first
	}
	if l.count == len(l.ring) {
		grown := make([]time.Time, min(l.limit, max(16, 2*l.count)))
		n := copy(grown, l.ring[l.head:])
		copy(grown[n:], l.ring[:l.head])
		l.ring, l.head = grown, 0
	}
	l.ring[(l.head+l.count)%len(l.ring)] = now
	l.count++
	l.limited = false
	return now, true, false
}

func (l *reportLimiter) release(slot time.Time) {
	if l == nil || slot.IsZero() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	at := func(p int) *time.Time { return &l.ring[(l.head+p)%len(l.ring)] }
	for p := l.count - 1; p >= 0; p-- {
		if at(p).Before(slot) {
			return
		}
		if at(p).Equal(slot) {
			for ; p < l.count-1; p++ {
				*at(p) = *at(p + 1)
			}
			l.count--
			return
		}
	}
}
