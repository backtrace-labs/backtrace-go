package bt

import "sync/atomic"

// dropReason indexes internalStats.drops. Keep dropReasonCount last.
type dropReason int

const (
	dropSampled dropReason = iota
	dropQueueFull
	dropClosed
	dropBeforeSend
	dropSerialization
	dropOversize
	dropRateLimit
	dropNetwork
	dropServerReject
	dropInternal
	dropReasonCount
)

// internalStats backs the ClientStats snapshot with lock-free counters.
type internalStats struct {
	accepted  atomic.Uint64
	delivered atomic.Uint64
	drops     [dropReasonCount]atomic.Uint64
}

func (s *internalStats) drop(r dropReason) {
	s.drops[r].Add(1)
}

func (s *internalStats) droppedTotal() uint64 {
	var total uint64
	for i := range s.drops {
		total += s.drops[i].Load()
	}
	return total
}

// ClientStats is an immutable snapshot of a client's report accounting,
// broken down by outcome. Accepted counts reports admitted to the queue;
// Delivered counts successful submissions; the remaining fields count
// discarded reports by reason.
type ClientStats struct {
	Accepted      uint64
	Delivered     uint64
	Sampled       uint64
	QueueFull     uint64
	Closed        uint64
	BeforeSend    uint64
	Serialization uint64
	Oversize      uint64
	RateLimited   uint64
	Network       uint64
	ServerReject  uint64
	Internal      uint64
}

func (s *internalStats) snapshot() ClientStats {
	return ClientStats{
		Accepted:      s.accepted.Load(),
		Delivered:     s.delivered.Load(),
		Sampled:       s.drops[dropSampled].Load(),
		QueueFull:     s.drops[dropQueueFull].Load(),
		Closed:        s.drops[dropClosed].Load(),
		BeforeSend:    s.drops[dropBeforeSend].Load(),
		Serialization: s.drops[dropSerialization].Load(),
		Oversize:      s.drops[dropOversize].Load(),
		RateLimited:   s.drops[dropRateLimit].Load(),
		Network:       s.drops[dropNetwork].Load(),
		ServerReject:  s.drops[dropServerReject].Load(),
		Internal:      s.drops[dropInternal].Load(),
	}
}
