package queue

import (
	"runtime"
	"sync"
)

type Queue struct {
	max       int
	active    []queueItem
	queued    []queueItem
	maxQueued int

	mu sync.Mutex
}

type queueItem struct {
	ch chan Status
}

type Handle struct {
	C <-chan Status
	i *internalHandle
}

type internalHandle struct {
	ch       chan Status
	q        *Queue
	released bool // Ensure .Release() is idempotent
}

type Status struct {
	Index int
	Max   int
	Ok    bool
	Full  bool
}

func New(max, maxQueued int) *Queue {
	return &Queue{max: max, maxQueued: maxQueued}
}

func (q *Queue) GetMax() int {
	q.mu.Lock()
	ret := q.max
	q.mu.Unlock()
	return ret
}

func (q *Queue) SetMax(max, maxQueued int) {
	q.mu.Lock()
	q.max, q.maxQueued = max, maxQueued
	q.mu.Unlock()
}

func (q *Queue) Acquire() *Handle {
	q.mu.Lock()
	defer q.mu.Unlock()

	ch := make(chan Status, 1)
	switch {
	case len(q.active) < q.max || q.max == 0: // q.max == 0 => no limit
		q.active = append(q.active, queueItem{ch})
		ch <- Status{Ok: true}
		close(ch)
	case q.maxQueued > 0 && len(q.queued) >= q.maxQueued:
		ch <- Status{Full: true}
		close(ch)
	default: // q.active >= q.max
		q.queued = append(q.queued, queueItem{ch})
		surplus := len(q.active) - q.max
		ch <- Status{Index: surplus + len(q.queued) - 1, Max: surplus + len(q.queued)}
	}
	return q.makeHandle(ch)
}

func (q *Queue) makeHandle(ch chan Status) *Handle {
	h := &Handle{
		C: ch,
		i: &internalHandle{
			ch: ch,
			q:  q,
		},
	}
	runtime.AddCleanup(h, (*internalHandle).release, h.i)
	return h
}

// Move next queued handle to active queued
func (q *Queue) popHead() {
	head := q.queued[0]
	head.ch <- Status{Ok: true}
	close(head.ch)
	q.active = append(q.active, head)
	q.queued = q.queued[1:]
}

func (q *Queue) releaseFromHandle(h *internalHandle) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Drain the channel as Release() is called by the channel reader thread
	go func() {
		for range h.ch {
		}
	}()

	// Remove this handle from queued list
	newList := make([]queueItem, 0, len(q.queued))
	for _, item := range q.queued {
		if h.ch == item.ch {
			close(h.ch)
			continue
		}
		newList = append(newList, item)
	}
	if len(newList) != len(q.queued) {
		q.queued = newList
		q.broadcastStatus()
		return
	}

	// Remove this handle from active list
	newList = make([]queueItem, 0, len(q.active))
	for _, item := range q.active {
		if h.ch == item.ch {
			// h.ch is already closed if it's in active list
			continue
		}
		newList = append(newList, item)
	}
	q.active = newList

	for len(q.queued) > 0 && (len(q.active) < q.max || q.max == 0) {
		q.popHead()
	}
	q.broadcastStatus()
}

// Release signals that one job is done
func (h *Handle) Release() {
	h.i.release()
}

func (h *internalHandle) release() {
	if h.released {
		return
	}
	h.released = true
	h.q.releaseFromHandle(h)
}

// Must be called with q.mu held
func (q *Queue) broadcastStatus() {
	surplus := len(q.active) - q.max
	for i := range q.queued {
		q.queued[i].ch <- Status{Index: surplus + i, Max: surplus + len(q.queued)}
	}
}
