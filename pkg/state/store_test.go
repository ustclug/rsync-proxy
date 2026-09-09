package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func openTestStore(t *testing.T, dir, id string) *Store {
	t.Helper()
	s, err := Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.Register(Generation{ID: id, PID: os.Getpid()}))
	return s
}

func TestGlobalFIFOAndPolicy(t *testing.T) {
	dir := t.TempDir()
	a := openTestStore(t, dir, "a")
	b := openTestStore(t, dir, "b")
	ctx := context.Background()
	limits := []Limit{{Name: "u", Active: 1, Queued: 2, PerIP: 2}}
	require.NoError(t, a.Activate("a", "", limits))
	one, err := a.Acquire(ctx, "a", "u", "ip")
	require.NoError(t, err)
	require.True(t, one.Active)
	two, err := b.Acquire(ctx, "b", "u", "ip")
	require.NoError(t, err)
	require.False(t, two.Active)
	_, err = b.Acquire(ctx, "b", "u", "ip")
	require.ErrorIs(t, err, ErrPerIP)
	three, err := a.Acquire(ctx, "a", "u", "other")
	require.NoError(t, err)
	require.False(t, three.Active)
	_, err = b.Acquire(ctx, "b", "u", "third")
	require.ErrorIs(t, err, ErrFull)
	require.NoError(t, b.Activate("b", "a", limits))
	require.ErrorIs(t, a.SetLimits("a", limits), ErrNotCurrent)
	require.NoError(t, a.Release(ctx, "a", one.ID))
	bt, err := b.Poll("b")
	require.NoError(t, err)
	require.True(t, bt[two.ID].Active)
	at, err := a.Poll("a")
	require.NoError(t, err)
	require.False(t, at[three.ID].Active)
	require.NoError(t, b.Finish("b"))
	at, err = a.Poll("a")
	require.NoError(t, err)
	require.True(t, at[three.ID].Active)
	require.NoError(t, a.Release(ctx, "a", three.ID))
	require.NoError(t, a.Release(ctx, "a", three.ID))
}

func TestLowerLimitAndUnlimitedQueue(t *testing.T) {
	s := openTestStore(t, t.TempDir(), "a")
	ctx := context.Background()
	require.NoError(t, s.Activate("a", "", []Limit{{Name: "u", Active: 2}}))
	one, err := s.Acquire(ctx, "a", "u", "ip")
	require.NoError(t, err)
	two, err := s.Acquire(ctx, "a", "u", "ip")
	require.NoError(t, err)
	require.NoError(t, s.SetLimits("a", []Limit{{Name: "u", Active: 1}}))
	for range 50 {
		ticket, err := s.Acquire(ctx, "a", "u", "ip")
		require.NoError(t, err)
		require.False(t, ticket.Active)
	}
	require.NoError(t, s.Release(ctx, "a", one.ID))
	q, err := s.Queues()
	require.NoError(t, err)
	require.Equal(t, 1, q[0].ActiveCount)
	require.Equal(t, 50, q[0].QueuedCount)
	require.NoError(t, s.Release(ctx, "a", two.ID))
	q, err = s.Queues()
	require.NoError(t, err)
	require.Equal(t, 1, q[0].ActiveCount)
	require.Equal(t, 49, q[0].QueuedCount)
}

func TestSchemaRejectionAndBusy(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, "a")
	_, err := s.db.Exec("PRAGMA user_version=999")
	require.NoError(t, err)
	_, err = Open(dir)
	require.ErrorContains(t, err, "incompatible state schema")
	_, err = s.db.Exec("PRAGMA user_version=1")
	require.NoError(t, err)
	b := openTestStore(t, dir, "b")
	require.NoError(t, s.Activate("a", "", []Limit{{Name: "u", Active: 1}}))
	c, err := s.db.Conn(context.Background())
	require.NoError(t, err)
	_, err = c.ExecContext(context.Background(), "BEGIN IMMEDIATE")
	require.NoError(t, err)
	_, err = b.Acquire(context.Background(), "b", "u", "ip")
	require.Error(t, err)
	_, err = c.ExecContext(context.Background(), "ROLLBACK")
	require.NoError(t, err)
	require.NoError(t, c.Close())
	q, err := s.Queues()
	require.NoError(t, err)
	require.Zero(t, q[0].ActiveCount)
}

func TestStateWorker(t *testing.T) {
	dir := os.Getenv("RSYNC_STATE_TEST_DIR")
	if dir == "" {
		return
	}
	id := fmt.Sprint(os.Getpid())
	s, err := Open(dir)
	if err != nil {
		panic(fmt.Errorf("open state store: %w", err))
	}
	defer s.Close()
	lock, err := Lock(filepath.Join(dir, id+".lock"))
	if err != nil {
		panic(fmt.Errorf("lock generation: %w", err))
	}
	defer lock.Close()
	if err = s.Register(Generation{ID: id, PID: os.Getpid()}); err != nil {
		panic(fmt.Errorf("register generation: %w", err))
	}
	ctx := context.Background()
	for range 40 {
		ticket, err := s.Acquire(ctx, id, "u", id)
		if errors.Is(err, ErrFull) || errors.Is(err, ErrPerIP) {
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			panic(fmt.Errorf("acquire ticket: %w", err))
		}
		for !ticket.Active {
			time.Sleep(time.Millisecond)
			ts, err := s.Poll(id)
			if err != nil {
				panic(fmt.Errorf("poll tickets: %w", err))
			}
			ticket = ts[ticket.ID]
		}
		time.Sleep(2 * time.Millisecond)
		if err = s.Release(ctx, id, ticket.ID); err != nil {
			panic(fmt.Errorf("release ticket %d: %w", ticket.ID, err))
		}
	}
}

func TestConcurrentProcessAdmission(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, "parent")
	require.NoError(t, s.Activate("parent", "", []Limit{{Name: "u", Active: 2, Queued: 20, PerIP: 2}}))
	executable, err := os.Executable()
	require.NoError(t, err)
	type worker struct {
		cmd    *exec.Cmd
		output *bytes.Buffer
	}
	workers := make([]worker, 0, 4)
	for range 4 {
		c := exec.Command(executable, "-test.run=^TestStateWorker$")
		c.Env = append(os.Environ(), "RSYNC_STATE_TEST_DIR="+dir)
		output := new(bytes.Buffer)
		c.Stdout = output
		c.Stderr = output
		require.NoError(t, c.Start())
		workers = append(workers, worker{cmd: c, output: output})
	}
	var wg sync.WaitGroup
	type workerResult struct {
		pid    int
		err    error
		output string
	}
	results := make(chan workerResult, len(workers))
	for _, w := range workers {
		wg.Go(func() {
			err := w.cmd.Wait()
			results <- workerResult{pid: w.cmd.Process.Pid, err: err, output: w.output.String()}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			for range workers {
				result := <-results
				if result.err != nil {
					t.Errorf("worker PID %d failed: %v\n%s", result.pid, result.err, result.output)
				}
			}
			return
		case <-ticker.C:
			qs, err := s.Queues()
			require.NoError(t, err)
			require.LessOrEqual(t, qs[0].ActiveCount, 2)
		}
	}
}

func TestAdmissionAt100ConnectionsPerSecond(t *testing.T) {
	s := openTestStore(t, t.TempDir(), "a")
	require.NoError(t, s.Activate("a", "", []Limit{{Name: "u", Active: 10, Queued: 100}}))
	latencies := make([]time.Duration, 0, 200)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for range 200 {
		<-ticker.C
		start := time.Now()
		ticket, err := s.Acquire(context.Background(), "a", "u", "ip")
		require.NoError(t, err)
		latencies = append(latencies, time.Since(start))
		require.NoError(t, s.Release(context.Background(), "a", ticket.ID))
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("100 connections/s, admission p50=%s p99=%s max=%s", latencies[100], latencies[198], latencies[199])
}

func TestQueueBurst(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, "a")
	_ = openTestStore(t, dir, "b")
	ctx := context.Background()
	require.NoError(t, s.Activate("a", "", []Limit{{Name: "u", Active: 1, Queued: 1000}}))
	active, err := s.Acquire(ctx, "a", "u", "ip")
	require.NoError(t, err)
	var first Ticket
	for i := range 1000 {
		id := "a"
		if i%2 == 0 {
			id = "b"
		}
		ticket, err := s.Acquire(ctx, id, "u", "ip")
		require.NoError(t, err)
		require.False(t, ticket.Active)
		if i == 0 {
			first = ticket
		}
	}
	start := time.Now()
	tickets, err := s.Poll("b")
	require.NoError(t, err)
	t.Logf("1000 queued connections: batched poll took %s", time.Since(start))
	require.Len(t, tickets, 500)
	require.Zero(t, tickets[first.ID].Index)
	require.Equal(t, 1000, tickets[first.ID].Queued)
	require.NoError(t, s.Release(ctx, "a", active.ID))
	tickets, err = s.Poll("b")
	require.NoError(t, err)
	require.True(t, tickets[first.ID].Active)
}
