package server

import (
	"context"
	"testing"
	"time"
)

func TestNewQueueManager(t *testing.T) {
	qm := NewQueueManager()
	if qm == nil {
		t.Fatal("NewQueueManager returned nil")
	}
	if qm.queues == nil {
		t.Fatal("queues map not initialized")
	}
}

func TestRegisterAndUnregisterUpstream(t *testing.T) {
	qm := NewQueueManager()

	// Test registration
	qm.RegisterUpstream("127.0.0.1:1234", 10)
	addrs := qm.ListAddresses()
	if len(addrs) != 1 || addrs[0] != "127.0.0.1:1234" {
		t.Errorf("Expected 1 address, got %v", addrs)
	}

	// Test unregistration
	qm.UnregisterUpstream("127.0.0.1:1234")
	addrs = qm.ListAddresses()
	if len(addrs) != 0 {
		t.Errorf("Expected 0 addresses after unregister, got %v", addrs)
	}
}

func TestAcquireConnNoLimit(t *testing.T) {
	qm := NewQueueManager()
	qm.RegisterUpstream("127.0.0.1:1234", 0) // 0 means unlimited

	info := &ConnInfo{Index: 1}

	// Should always acquire when no limit
	for i := 0; i < 100; i++ {
		if !qm.AcquireConn("127.0.0.1:1234", info) {
			t.Errorf("Should acquire connection when no limit, iteration %d", i)
		}
	}
}

func TestAcquireConnWithLimit(t *testing.T) {
	qm := NewQueueManager()
	qm.RegisterUpstream("127.0.0.1:1234", 2)

	// First 2 should acquire immediately
	info1 := &ConnInfo{Index: 1}
	info2 := &ConnInfo{Index: 2}

	if !qm.AcquireConn("127.0.0.1:1234", info1) {
		t.Error("First connection should acquire immediately")
	}
	if !qm.AcquireConn("127.0.0.1:1234", info2) {
		t.Error("Second connection should acquire immediately")
	}

	// Third should fail and need to wait
	info3 := &ConnInfo{Index: 3}
	if qm.AcquireConn("127.0.0.1:1234", info3) {
		t.Error("Third connection should not acquire when limit reached")
	}

	// Check queue info
	active, max, waiting := qm.GetQueueInfo("127.0.0.1:1234")
	if active != 2 {
		t.Errorf("Expected 2 active connections, got %d", active)
	}
	if max != 2 {
		t.Errorf("Expected max 2, got %d", max)
	}
	if waiting != 1 {
		t.Errorf("Expected 1 waiting connection, got %d", waiting)
	}
}

func TestReleaseConn(t *testing.T) {
	qm := NewQueueManager()
	qm.RegisterUpstream("127.0.0.1:1234", 1)

	info := &ConnInfo{Index: 1}
	qm.AcquireConn("127.0.0.1:1234", info)

	active, _, _ := qm.GetQueueInfo("127.0.0.1:1234")
	if active != 1 {
		t.Errorf("Expected 1 active connection before release, got %d", active)
	}

	qm.ReleaseConn("127.0.0.1:1234")

	active, _, _ = qm.GetQueueInfo("127.0.0.1:1234")
	if active != 0 {
		t.Errorf("Expected 0 active connections after release, got %d", active)
	}
}

func TestWaitInQueue(t *testing.T) {
	qm := NewQueueManager()
	qm.RegisterUpstream("127.0.0.1:1234", 1)

	// Acquire the only slot
	info1 := &ConnInfo{Index: 1}
	qm.AcquireConn("127.0.0.1:1234", info1)

	// Try to wait in queue
	info2 := &ConnInfo{Index: 2}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	updateCalled := false
	errChan := make(chan error, 1)

	go func() {
		err := qm.WaitInQueue(ctx, "127.0.0.1:1234", info2, func(pos int) {
			updateCalled = true
			t.Logf("Queue position updated: %d", pos)
		})
		errChan <- err
	}()

	// Wait a bit then release the first connection
	time.Sleep(50 * time.Millisecond)
	qm.ReleaseConn("127.0.0.1:1234")

	// Wait for the goroutine to complete
	err := <-errChan
	if err != nil {
		t.Errorf("WaitInQueue returned error: %v", err)
	}

	if !updateCalled {
		t.Error("Update callback was not called")
	}
}

func TestWaitInQueueContextCancel(t *testing.T) {
	qm := NewQueueManager()
	qm.RegisterUpstream("127.0.0.1:1234", 1)

	// Acquire the only slot
	info1 := &ConnInfo{Index: 1}
	qm.AcquireConn("127.0.0.1:1234", info1)

	// Try to wait in queue with a context that will be canceled
	info2 := &ConnInfo{Index: 2}
	ctx, cancel := context.WithCancel(context.Background())

	errChan := make(chan error, 1)
	go func() {
		err := qm.WaitInQueue(ctx, "127.0.0.1:1234", info2, func(pos int) {
			t.Logf("Queue position: %d", pos)
		})
		errChan <- err
	}()

	// Cancel the context
	time.Sleep(10 * time.Millisecond)
	cancel()

	err := <-errChan
	if err != context.Canceled {
		t.Errorf("Expected context.Canceled, got %v", err)
	}
}

func TestGetAllQueueInfo(t *testing.T) {
	qm := NewQueueManager()
	qm.RegisterUpstream("127.0.0.1:1234", 5)
	qm.RegisterUpstream("127.0.0.1:1235", 0) // unlimited, should be skipped
	qm.RegisterUpstream("127.0.0.1:1236", 10)

	// Acquire some connections
	qm.AcquireConn("127.0.0.1:1234", &ConnInfo{Index: 1})
	qm.AcquireConn("127.0.0.1:1234", &ConnInfo{Index: 2})
	qm.AcquireConn("127.0.0.1:1236", &ConnInfo{Index: 3})

	info := qm.GetAllQueueInfo()

	if len(info) != 2 {
		t.Errorf("Expected 2 upstreams in info (excluding unlimited), got %d", len(info))
	}

	if q, ok := info["127.0.0.1:1234"]; ok {
		if q.Active != 2 {
			t.Errorf("Expected 2 active for 127.0.0.1:1234, got %d", q.Active)
		}
		if q.Max != 5 {
			t.Errorf("Expected max 5 for 127.0.0.1:1234, got %d", q.Max)
		}
	} else {
		t.Error("127.0.0.1:1234 not found in queue info")
	}

	if q, ok := info["127.0.0.1:1236"]; ok {
		if q.Active != 1 {
			t.Errorf("Expected 1 active for 127.0.0.1:1236, got %d", q.Active)
		}
		if q.Max != 10 {
			t.Errorf("Expected max 10 for 127.0.0.1:1236, got %d", q.Max)
		}
	} else {
		t.Error("127.0.0.1:1236 not found in queue info")
	}
}

func TestGetQueueInfoNonExistent(t *testing.T) {
	qm := NewQueueManager()

	active, max, waiting := qm.GetQueueInfo("non-existent:1234")
	if active != 0 || max != 0 || waiting != 0 {
		t.Errorf("Expected all zeros for non-existent upstream, got active=%d, max=%d, waiting=%d", active, max, waiting)
	}
}
