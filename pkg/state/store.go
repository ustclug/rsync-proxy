// Package state coordinates worker generations through the supervisor's memory.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

const Protocol = 2

const (
	Starting = "starting"
	Serving  = "serving"
	Draining = "draining"
	Exited   = "exited"
)

var (
	ErrPerIP      = errors.New("per-IP connection limit reached")
	ErrFull       = errors.New("upstream queue is full")
	ErrNotCurrent = errors.New("generation is no longer current")
)

type Limit struct {
	Name                  string
	Active, Queued, PerIP int
}

type Generation struct {
	ID       string          `json:"id"`
	PID      int             `json:"pid"`
	Version  string          `json:"version"`
	Control  string          `json:"control"`
	State    string          `json:"state"`
	Deadline int64           `json:"deadline,omitempty"`
	Snapshot json.RawMessage `json:"-"`
}

type Ticket struct {
	ID                 int64
	Active             bool
	Index, Queued, Max int
}

type QueueInfo struct {
	Limit
	ActiveCount, QueuedCount int
}

// Store is either the supervisor's in-memory store or a Unix socket client.
// The mutex covers only memory operations; no I/O is performed while holding it.
type Store struct {
	mu          sync.Mutex
	current     string
	generations map[string]Generation
	order       []string
	limits      map[string]Limit
	tickets     map[int64]entry
	next        int64
	revision    uint64
	changed     chan struct{}
	remote      *remoteStore
}

type entry struct {
	Ticket
	Generation, Upstream, IP string
}

func New() *Store {
	return &Store{generations: make(map[string]Generation), limits: make(map[string]Limit), tickets: make(map[int64]entry), changed: make(chan struct{})}
}

func (s *Store) Close() error {
	if s.remote != nil {
		return s.remote.close()
	}
	return nil
}

func (s *Store) changedLocked() {
	s.revision++
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Store) Current() (string, error) {
	if s.remote != nil {
		r, e := s.call(Request{Op: "current"})
		return r.Current, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, nil
}

func (s *Store) Register(g Generation) error {
	if s.remote != nil {
		_, e := s.call(Request{Op: "register", Generation: g})
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if g.ID == "" {
		return errors.New("empty generation")
	}
	if _, ok := s.generations[g.ID]; ok {
		return errors.New("generation already registered")
	}
	g.State = Starting
	s.generations[g.ID] = g
	s.order = append(s.order, g.ID)
	return nil
}

func validateLimits(limits []Limit) error {
	for _, l := range limits {
		if l.Name == "" || l.Active < 0 || l.Queued < 0 || l.PerIP < 0 {
			return errors.New("invalid admission limit")
		}
	}
	return nil
}

func (s *Store) Activate(id, parent string, limits []Limit) error {
	if s.remote != nil {
		_, e := s.call(Request{Op: "activate", ID: id, Parent: parent, Limits: limits})
		return e
	}
	if err := validateLimits(limits); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != parent {
		return ErrNotCurrent
	}
	g, ok := s.generations[id]
	if !ok || g.State == Exited {
		return errors.New("unknown or exited generation")
	}
	for _, l := range limits {
		s.limits[l.Name] = l
	}
	s.current = id
	g.State = Serving
	g.Deadline = 0
	s.generations[id] = g
	s.promoteLocked()
	s.changedLocked()
	return nil
}

func (s *Store) SetLimits(id string, limits []Limit) error {
	if s.remote != nil {
		_, e := s.call(Request{Op: "limits", ID: id, Limits: limits})
		return e
	}
	if err := validateLimits(limits); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != id {
		return ErrNotCurrent
	}
	for _, l := range limits {
		s.limits[l.Name] = l
	}
	s.promoteLocked()
	s.changedLocked()
	return nil
}

func (s *Store) sortedLocked() []int64 {
	ids := make([]int64, 0, len(s.tickets))
	for id := range s.tickets {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (s *Store) promoteLocked() {
	active := make(map[string]int)
	for _, t := range s.tickets {
		if t.Active {
			active[t.Upstream]++
		}
	}
	for _, id := range s.sortedLocked() {
		t := s.tickets[id]
		l := s.limits[t.Upstream]
		if !t.Active && (l.Active == 0 || active[t.Upstream] < l.Active) {
			t.Active = true
			s.tickets[id] = t
			active[t.Upstream]++
		}
	}
}

func (s *Store) Acquire(ctx context.Context, generation, upstream, ip string) (Ticket, error) {
	if err := ctx.Err(); err != nil {
		return Ticket{}, err
	}
	if s.remote != nil {
		// Once sent, await the definite result. Cancellation must not orphan a grant.
		r, e := s.call(Request{Op: "acquire", ID: generation, Upstream: upstream, IP: ip})
		return r.Ticket, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.generations[generation]
	if !ok || g.State == Exited {
		return Ticket{}, errors.New("unknown or exited generation")
	}
	l, ok := s.limits[upstream]
	if !ok {
		return Ticket{}, fmt.Errorf("unknown upstream %q", upstream)
	}
	active, queued, ips := 0, 0, 0
	for _, t := range s.tickets {
		if t.Upstream != upstream {
			continue
		}
		if t.IP == ip {
			ips++
		}
		if t.Active {
			active++
		} else {
			queued++
		}
	}
	if l.PerIP > 0 && ips >= l.PerIP {
		return Ticket{}, ErrPerIP
	}
	granted := queued == 0 && (l.Active == 0 || active < l.Active)
	if !granted && l.Queued > 0 && queued >= l.Queued {
		return Ticket{}, ErrFull
	}
	s.next++
	t := Ticket{ID: s.next, Active: granted, Index: queued, Queued: queued + 1, Max: l.Active}
	s.tickets[t.ID] = entry{Ticket: t, Generation: generation, Upstream: upstream, IP: ip}
	s.changedLocked()
	return t, nil
}

func (s *Store) Release(ctx context.Context, generation string, id int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.remote != nil {
		_, e := s.call(Request{Op: "release", ID: generation, TicketID: id})
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tickets[id]; ok && t.Generation == generation {
		delete(s.tickets, id)
		s.promoteLocked()
		s.changedLocked()
	}
	return nil
}

func (s *Store) Poll(generation string) (map[int64]Ticket, error) {
	if s.remote != nil {
		r, e := s.call(Request{Op: "poll", ID: generation})
		return r.Tickets, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pollLocked(generation), nil
}

func (s *Store) pollLocked(generation string) map[int64]Ticket {
	queued := make(map[string]int)
	position := make(map[string]int)
	for _, t := range s.tickets {
		if !t.Active {
			queued[t.Upstream]++
		}
	}
	result := make(map[int64]Ticket)
	for _, id := range s.sortedLocked() {
		t := s.tickets[id]
		if t.Generation == generation {
			v := t.Ticket
			v.Index = position[t.Upstream]
			v.Queued = queued[t.Upstream]
			v.Max = s.limits[t.Upstream].Active
			result[id] = v
		}
		if !t.Active {
			position[t.Upstream]++
		}
	}
	return result
}

// Watch waits for an admission change. The revision closes the race between
// taking a snapshot and subscribing. No periodic queue scan is necessary.
func (s *Store) Watch(ctx context.Context, after uint64) (uint64, error) {
	if s.remote != nil {
		return s.watchRemote(ctx, after)
	}
	for {
		s.mu.Lock()
		rev, ch := s.revision, s.changed
		s.mu.Unlock()
		if rev != after {
			return rev, nil
		}
		select {
		case <-ctx.Done():
			return after, ctx.Err()
		case <-ch:
		}
	}
}

func (s *Store) Queues() ([]QueueInfo, error) {
	if s.remote != nil {
		r, e := s.call(Request{Op: "queues"})
		return r.Queues, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := make(map[string]QueueInfo)
	for name, l := range s.limits {
		counts[name] = QueueInfo{Limit: l}
	}
	for _, t := range s.tickets {
		q := counts[t.Upstream]
		if t.Active {
			q.ActiveCount++
		} else {
			q.QueuedCount++
		}
		counts[t.Upstream] = q
	}
	result := make([]QueueInfo, 0, len(counts))
	for _, q := range counts {
		result = append(result, q)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (s *Store) Generations() ([]Generation, error) {
	if s.remote != nil {
		r, e := s.call(Request{Op: "generations"})
		return r.Generations, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Generation, 0, len(s.order))
	for _, id := range s.order {
		g := s.generations[id]
		g.Snapshot = append(json.RawMessage(nil), g.Snapshot...)
		result = append(result, g)
	}
	return result, nil
}

func (s *Store) Publish(id string, snapshot []byte) error {
	if s.remote != nil {
		_, e := s.call(Request{Op: "publish", ID: id, Snapshot: snapshot})
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.generations[id]
	if !ok {
		return errors.New("unknown generation")
	}
	g.Snapshot = append(json.RawMessage(nil), snapshot...)
	s.generations[id] = g
	return nil
}

func (s *Store) Drain(id string, deadline time.Time) error {
	if s.remote != nil {
		_, e := s.call(Request{Op: "drain", ID: id, Deadline: deadline})
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.generations[id]
	if !ok {
		return errors.New("unknown generation")
	}
	if g.State == Serving {
		g.State = Draining
		if !deadline.IsZero() {
			g.Deadline = deadline.Unix()
		}
		s.generations[id] = g
	}
	return nil
}

// Finish is called by the supervisor only after Wait confirms process death.
// Losing an IPC connection alone is not proof that its transfers have ended.
func (s *Store) Finish(id string) error {
	if s.remote != nil {
		return errors.New("only the supervisor can retire workers")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.generations[id]
	if !ok {
		return nil
	}
	for key, t := range s.tickets {
		if t.Generation == id {
			delete(s.tickets, key)
		}
	}
	g.State = Exited
	s.generations[id] = g
	s.promoteLocked()
	s.changedLocked()
	return nil
}
