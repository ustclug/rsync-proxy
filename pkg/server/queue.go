package server

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// QueueManager manages connection limits and queuing for each upstream
type QueueManager struct {
	mu     sync.RWMutex
	queues map[string]*UpstreamQueue
}

// UpstreamQueue manages the connection pool for a single upstream
type UpstreamQueue struct {
	address     string
	maxConn     int
	activeConn  atomic.Int32
	waitingConn sync.Map // connIndex -> *ConnInfo
	notifyCh    chan struct{}
}

// NewQueueManager creates a new queue manager
func NewQueueManager() *QueueManager {
	return &QueueManager{
		queues: make(map[string]*UpstreamQueue),
	}
}

// RegisterUpstream registers an upstream with its connection limit
// If maxConn <= 0, no limit is enforced
func (qm *QueueManager) RegisterUpstream(address string, maxConn int) {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	qm.queues[address] = &UpstreamQueue{
		address:  address,
		maxConn:  maxConn,
		notifyCh: make(chan struct{}, 1),
	}
}

// UnregisterUpstream removes an upstream from the manager
func (qm *QueueManager) UnregisterUpstream(address string) {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	if q, ok := qm.queues[address]; ok {
		close(q.notifyCh)
		delete(qm.queues, address)
	}
}

// ListAddresses returns all registered upstream addresses
func (qm *QueueManager) ListAddresses() []string {
	qm.mu.RLock()
	defer qm.mu.RUnlock()

	addrs := make([]string, 0, len(qm.queues))
	for addr := range qm.queues {
		addrs = append(addrs, addr)
	}
	return addrs
}

// AcquireConn attempts to acquire a connection slot for the given upstream
// Returns true if acquired immediately, false if the caller needs to wait in queue
func (qm *QueueManager) AcquireConn(address string, info *ConnInfo) bool {
	qm.mu.RLock()
	q, ok := qm.queues[address]
	qm.mu.RUnlock()

	if !ok || q.maxConn <= 0 {
		// No limit for this upstream
		return true
	}

	active := q.activeConn.Load()
	if active < int32(q.maxConn) {
		// Slot available
		q.activeConn.Add(1)
		return true
	}

	// Need to wait in queue
	q.waitingConn.Store(info.Index, info)
	return false
}

// ReleaseConn releases a connection slot and notifies waiting connections
func (qm *QueueManager) ReleaseConn(address string) {
	qm.mu.RLock()
	q, ok := qm.queues[address]
	qm.mu.RUnlock()

	if !ok || q.maxConn <= 0 {
		return
	}

	q.activeConn.Add(-1)

	// Notify one waiter
	select {
	case q.notifyCh <- struct{}{}:
	default:
	}
}

// WaitInQueue waits in the queue for a connection slot
// Calls onUpdate callback whenever queue position changes
// Returns context.Canceled if the context is canceled
func (qm *QueueManager) WaitInQueue(ctx context.Context, address string, info *ConnInfo, onUpdate func(pos int)) error {
	qm.mu.RLock()
	q, ok := qm.queues[address]
	qm.mu.RUnlock()

	if !ok || q.maxConn <= 0 {
		return fmt.Errorf("upstream not found or no limit: %s", address)
	}

	// Register in waiting queue
	q.waitingConn.Store(info.Index, info)
	defer q.waitingConn.Delete(info.Index)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		// Check current position
		pos := q.getQueuePosition(info.Index)
		onUpdate(pos)

		// Try to acquire
		active := q.activeConn.Load()
		if active < int32(q.maxConn) {
			// Slot available, try to acquire atomically
			if q.activeConn.CompareAndSwap(active, active+1) {
				return nil
			}
		}

		// Wait for notification or ticker
		select {
		case <-ticker.C:
			// Continue loop, will check position and call onUpdate
		case <-q.notifyCh:
			// A connection was released, try again immediately
		case <-ctx.Done():
			return context.Canceled
		}
	}
}

// getQueuePosition calculates the position in queue for a given connection index
// Returns 1-based position (1 means next in line)
func (q *UpstreamQueue) getQueuePosition(connIndex uint32) int {
	pos := 1
	q.waitingConn.Range(func(key, value interface{}) bool {
		idx := key.(uint32)
		if idx < connIndex {
			pos++
		}
		return true
	})
	return pos
}

// GetQueueInfo returns the current queue status for an upstream
func (qm *QueueManager) GetQueueInfo(address string) (active, max, waiting int) {
	qm.mu.RLock()
	q, ok := qm.queues[address]
	qm.mu.RUnlock()

	if !ok {
		return 0, 0, 0
	}

	active = int(q.activeConn.Load())
	max = q.maxConn

	// Count waiting connections
	q.waitingConn.Range(func(_, _ interface{}) bool {
		waiting++
		return true
	})

	return
}

// GetAllQueueInfo returns queue info for all upstreams
func (qm *QueueManager) GetAllQueueInfo() map[string]struct {
	Active  int
	Max     int
	Waiting int
} {
	result := make(map[string]struct {
		Active  int
		Max     int
		Waiting int
	})

	qm.mu.RLock()
	defer qm.mu.RUnlock()

	for addr, q := range qm.queues {
		if q.maxConn <= 0 {
			continue // Skip unlimited upstreams
		}

		active := int(q.activeConn.Load())
		waiting := 0
		q.waitingConn.Range(func(_, _ interface{}) bool {
			waiting++
			return true
		})

		result[addr] = struct {
			Active  int
			Max     int
			Waiting int
		}{
			Active:  active,
			Max:     q.maxConn,
			Waiting: waiting,
		}
	}

	return result
}
