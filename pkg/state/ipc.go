package state

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Request and Response form the versioned, private supervisor protocol.
// gob framing and request IDs are supplied by net/rpc on a persistent socket.
const operationUpgrade = "upgrade"

type Request struct {
	Protocol                     int
	Op, ID, Parent, Upstream, IP string
	Generation                   Generation
	Limits                       []Limit
	TicketID                     int64
	Snapshot                     []byte
	Deadline                     time.Time
	Revision                     uint64
	Timeout                      time.Duration
}
type Response struct {
	Error       string
	Current     string
	Ticket      Ticket
	Tickets     map[int64]Ticket
	Queues      []QueueInfo
	Generations []Generation
	Revision    uint64
}

// A stopped peer must not leave an RPC sender blocked indefinitely before its
// response timeout even starts. net/rpc serializes writes on each connection.
type deadlineConn struct{ net.Conn }

func (c deadlineConn) Write(b []byte) (int, error) {
	if err := c.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

type remoteStore struct {
	client *rpc.Client
	done   chan struct{}
	once   sync.Once
}

func (r *remoteStore) close() error {
	var err error
	r.once.Do(func() { close(r.done); err = r.client.Close() })
	return err
}

func Open(dir string) (*Store, error) {
	conn, err := net.DialTimeout("unix", filepath.Join(dir, "supervisor.sock"), 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to supervisor: %w", err)
	}
	s := &Store{remote: &remoteStore{client: rpc.NewClient(deadlineConn{conn}), done: make(chan struct{})}}
	if _, err = s.call(Request{Op: "hello"}); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// Done closes if the control connection fails. Workers must then stop serving;
// they cannot establish a fresh authority and silently forget existing grants.
func (s *Store) Done() <-chan struct{} {
	if s.remote == nil {
		return nil
	}
	return s.remote.done
}

func decodeError(msg string) error {
	switch msg {
	case "":
		return nil
	case ErrFull.Error():
		return ErrFull
	case ErrPerIP.Error():
		return ErrPerIP
	case ErrNotCurrent.Error():
		return ErrNotCurrent
	default:
		return errors.New(msg)
	}
}

func (s *Store) call(req Request) (Response, error) {
	req.Protocol = Protocol
	var result Response
	call := s.remote.client.Go("Coordinator.Call", req, &result, make(chan *rpc.Call, 1))
	timeout := 5 * time.Second
	if req.Op == operationUpgrade {
		timeout = req.Timeout + 5*time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case completed := <-call.Done:
		if completed.Error != nil {
			_ = s.Close()
			return Response{}, completed.Error
		}
		return result, decodeError(result.Error)
	case <-timer.C:
		_ = s.Close()
		return Response{}, errors.New("supervisor request timed out")
	}
}

func (s *Store) watchRemote(ctx context.Context, after uint64) (uint64, error) {
	for {
		var r Response
		call := s.remote.client.Go("Coordinator.Call", Request{Protocol: Protocol, Op: "watch", Revision: after}, &r, make(chan *rpc.Call, 1))
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return after, ctx.Err()
		case <-timer.C:
			_ = s.Close()
			return after, errors.New("supervisor watch timed out")
		case c := <-call.Done:
			timer.Stop()
			if c.Error != nil {
				_ = s.Close()
				return after, c.Error
			}
			if err := decodeError(r.Error); err != nil {
				return after, err
			}
			if r.Revision != after {
				return r.Revision, nil
			}
		}
	}
}

func (s *Store) Upgrade(id string, timeout time.Duration) error {
	if s.remote == nil {
		return errors.New("upgrade requires supervisor client")
	}
	_, e := s.call(Request{Op: operationUpgrade, ID: id, Timeout: timeout})
	return e
}

type coordinator struct {
	store   *Store
	upgrade func(string, time.Duration) error
}

func (c *coordinator) Call(q Request, r *Response) error {
	if q.Protocol != Protocol {
		r.Error = "incompatible supervisor protocol"
		return nil
	}
	var err error
	switch q.Op {
	case "hello":
	case "current":
		r.Current, err = c.store.Current()
	case "register":
		err = c.store.Register(q.Generation)
	case "activate":
		err = c.store.Activate(q.ID, q.Parent, q.Limits)
	case "limits":
		err = c.store.SetLimits(q.ID, q.Limits)
	case "acquire":
		r.Ticket, err = c.store.Acquire(context.Background(), q.ID, q.Upstream, q.IP)
	case "release":
		err = c.store.Release(context.Background(), q.ID, q.TicketID)
	case "poll":
		r.Tickets, err = c.store.Poll(q.ID)
	case "queues":
		r.Queues, err = c.store.Queues()
	case "generations":
		r.Generations, err = c.store.Generations()
	case "publish":
		err = c.store.Publish(q.ID, q.Snapshot)
	case "drain":
		err = c.store.Drain(q.ID, q.Deadline)
	case "watch":
		// Bound abandoned watches without changing their event-driven semantics.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r.Revision, err = c.store.Watch(ctx, q.Revision)
		if errors.Is(err, context.DeadlineExceeded) {
			err = nil
		}
	case operationUpgrade:
		if c.upgrade == nil {
			err = errors.New("upgrade unavailable")
		} else {
			err = c.upgrade(q.ID, q.Timeout)
		}
	default:
		err = fmt.Errorf("unsupported supervisor operation %q", q.Op)
	}
	if err != nil {
		r.Error = err.Error()
	}
	return nil
}

// Serve exposes a store owned by the supervisor. The caller must hold the
// instance lifetime lock before removing a stale socket or starting this server.
func Serve(dir string, s *Store, upgrade func(string, time.Duration) error) (func(), error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "supervisor.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		_ = l.Close()
		return nil, err
	}
	srv := rpc.NewServer()
	if err = srv.RegisterName("Coordinator", &coordinator{store: s, upgrade: upgrade}); err != nil {
		_ = l.Close()
		return nil, err
	}
	var mu sync.Mutex
	clients := make(map[net.Conn]bool)
	var wg sync.WaitGroup
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			conn, e := l.Accept()
			if e != nil {
				return
			}
			mu.Lock()
			clients[conn] = true
			mu.Unlock()
			wg.Go(func() {
				srv.ServeConn(deadlineConn{conn})
				mu.Lock()
				delete(clients, conn)
				mu.Unlock()
			})
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = l.Close()
			<-accepted
			mu.Lock()
			for c := range clients {
				_ = c.Close()
			}
			mu.Unlock()
			wg.Wait()
		})
	}, nil
}
