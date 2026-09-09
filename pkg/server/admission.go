package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ustclug/rsync-proxy/pkg/queue"
	"github.com/ustclug/rsync-proxy/pkg/state"
)

func sharedLimits(upstreams []upstreamConfig) []state.Limit {
	result := make([]state.Limit, 0, len(upstreams))
	for _, u := range upstreams {
		result = append(result, state.Limit{Name: u.Name, Active: u.MaxActiveConns, Queued: u.MaxQueuedConns, PerIP: u.PerIPMaxActiveConns})
	}
	return result
}

func (s *Server) Limits() []state.Limit {
	s.reloadLock.RLock()
	defer s.reloadLock.RUnlock()
	return sharedLimits(s.upstreams)
}

type admissionHandle struct {
	c       <-chan queue.Status
	release func()
}
type admissionWait struct {
	c    chan queue.Status
	last queue.Status
}

type admission struct {
	s        *Server
	mu       sync.Mutex
	waiting  map[int64]*admissionWait
	releases map[int64]bool
	done     chan struct{}
}

func (s *Server) StartAdmission() func() {
	a := &admission{s: s, waiting: make(map[int64]*admissionWait), releases: make(map[int64]bool), done: make(chan struct{})}
	s.admission = a
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(a.done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.poll()
			}
		}
	}()
	return func() { cancel(); <-a.done }
}

func offerStatus(c chan queue.Status, status queue.Status) {
	select {
	case <-c:
	default:
	}
	c <- status
}

func (a *admission) poll() {
	a.mu.Lock()
	ids := make([]int64, 0, len(a.releases))
	for id := range a.releases {
		ids = append(ids, id)
	}
	hasWaiting := len(a.waiting) > 0
	a.mu.Unlock()
	for _, id := range ids {
		a.release(id)
	}
	if !hasWaiting {
		return
	}
	tickets, err := a.s.Shared.Poll(a.s.Generation)
	if err != nil {
		log.Printf("[ERROR] poll admission: %v", err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, c := range a.waiting {
		t, ok := tickets[id]
		if !ok {
			continue
		}
		status := queue.Status{Ok: t.Active, Index: t.Index, Max: t.Queued}
		if status != c.last {
			offerStatus(c.c, status)
			c.last = status
		}
		if t.Active {
			delete(a.waiting, id)
		}
	}
}

func (a *admission) release(id int64) {
	a.mu.Lock()
	delete(a.waiting, id)
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := a.s.Shared.Release(ctx, a.s.Generation, id)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.releases[id] = true
		log.Printf("[ERROR] release admission (will retry): %v", err)
	} else {
		delete(a.releases, id)
	}
}

func (s *Server) acquire(ctx context.Context, upstream, ip string, q *queue.Queue) (admissionHandle, error) {
	if s.Shared == nil {
		counter := s.getPerIPCounter(upstream, ip)
		limit := s.getPerIPLimitForUpstream(upstream)
		if n := counter.Add(1); limit > 0 && n > int64(limit) {
			counter.Add(-1)
			return admissionHandle{}, fmt.Errorf("per-IP cap of %d reached: %w", limit, state.ErrPerIP)
		}
		h := q.Acquire()
		return admissionHandle{c: h.C, release: func() { h.Release(); counter.Add(-1) }}, nil
	}
	t, err := s.Shared.Acquire(ctx, s.Generation, upstream, ip)
	if err != nil && !errors.Is(err, state.ErrFull) {
		return admissionHandle{}, err
	}
	c := make(chan queue.Status, 1)
	if errors.Is(err, state.ErrFull) {
		c <- queue.Status{Full: true}
		return admissionHandle{c: c, release: func() {}}, nil
	}
	a := s.admission
	a.mu.Lock()
	if !t.Active {
		a.waiting[t.ID] = &admissionWait{c: c, last: queue.Status{Ok: t.Active, Index: t.Index, Max: t.Queued}}
	}
	c <- queue.Status{Ok: t.Active, Index: t.Index, Max: t.Queued}
	a.mu.Unlock()
	var once sync.Once
	return admissionHandle{c: c, release: func() { once.Do(func() { a.release(t.ID) }) }}, nil
}
