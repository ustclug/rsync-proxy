package queue

import (
	"sync"
)

type Queue struct {
	max     int
	current int
	list    []queueItem
	listMax int

	mu sync.Mutex
}

type queueItem struct {
	ch chan Status
}

type Status struct {
	Index int
	Max   int
	Ok    bool
	Full  bool
}

func New(max, maxQueued int) *Queue {
	return &Queue{max: max, listMax: maxQueued}
}

func (q *Queue) GetMax() int {
	q.mu.Lock()
	ret := q.max
	q.mu.Unlock()
	return ret
}

func (q *Queue) SetMax(max, maxQueued int) {
	q.mu.Lock()
	q.max, q.listMax = max, maxQueued
	q.mu.Unlock()
}

func (q *Queue) Acquire() <-chan Status {
	q.mu.Lock()
	defer q.mu.Unlock()

	ch := make(chan Status, 1)
	switch {
	case q.current < q.max || q.max == 0: // q.max == 0 => no limit
		q.current++
		ch <- q.makeOkStatus()
		close(ch)
	case q.listMax > 0 && len(q.list) >= q.listMax:
		ch <- Status{Full: true}
		close(ch)
	default: // q.current >= q.max
		q.list = append(q.list, queueItem{ch})
		surplus := q.current - q.max
		ch <- Status{Index: surplus + len(q.list) - 1, Max: surplus + len(q.list)}
	}
	return ch
}

// Release signals that one job is done
func (q *Queue) Release() {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.current <= q.max && len(q.list) > 0 {
		head := q.list[0]
		head.ch <- q.makeOkStatus()
		close(head.ch)

		q.list = q.list[1:]
		q.broadcastStatus()
	} else if q.current > 0 {
		q.current--
	}
}

// Abort aborts an item currently in queue
func (q *Queue) Abort(ch <-chan Status) {
	q.mu.Lock()
	defer q.mu.Unlock()

	newList := make([]queueItem, 0, len(q.list))
	for _, item := range q.list {
		if ch == (<-chan Status)(item.ch) {
			continue
		}
		newList = append(newList, item)
	}
	q.list = newList
	q.broadcastStatus()
}

func (q *Queue) broadcastStatus() {
	surplus := q.current - q.max
	for i := range q.list {
		if surplus+i < 0 {
			q.list[i].ch <- q.makeOkStatus()
		} else {
			q.list[i].ch <- Status{Index: surplus + i, Max: surplus + len(q.list)}
		}
	}
}

// Must be called with q.mu held, otherwise race condition may occur when reading q.list
func (q *Queue) makeOkStatus() Status {
	return Status{Ok: true, Max: len(q.list)}
}
