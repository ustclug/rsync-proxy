package queue

import (
	"sync"
)

type Queue struct {
	max     int
	current int
	list    []queueItem
	listMax int // not implemented

	mu sync.Mutex
}

type queueItem struct {
	ch chan Status
}

type Status struct {
	Index int
	Max   int
}

func (s Status) Ok() bool {
	return s.Index < 0
}

func New(max, maxQueued int) *Queue {
	return &Queue{max: max, listMax: maxQueued}
}

func (q *Queue) GetMax() int {
	// This is not protected by q.mu, but we don't expect racing calls between GetMax and SetMax.
	return q.max
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
	if q.current < q.max || q.max == 0 { // q.max == 0 => no limit
		q.current++
		ch <- q.makeOkStatus()
		close(ch)
	} else { // q.current >= q.max
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

	if len(q.list) > 0 {
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
		q.list[i].ch <- Status{Index: surplus + i, Max: surplus + len(q.list)}
	}
}

// Must be called with q.mu held, otherwise race condition may occur when reading q.list
func (q *Queue) makeOkStatus() Status {
	return Status{Index: -1, Max: len(q.list)}
}
