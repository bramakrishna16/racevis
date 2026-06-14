package racy

// Package racy contains intentional race conditions that mirror real bugs
// found in task queue and worker pool implementations.
//
// No external dependencies are imported — the patterns are simulated
// with equivalent in-memory structures so racevis can analyze them
// without requiring a running database.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- Simulated broker (mirrors PostgresBroker structure) ----

type TaskStatus string

const (
	StatusPending   TaskStatus = "pending"
	StatusClaimed   TaskStatus = "claimed"
	StatusCompleted TaskStatus = "completed"
	StatusFailed    TaskStatus = "failed"
)

type BrokerTask struct {
	ID         int
	Type       string
	RetryCount int
	MaxRetries int
	Status     TaskStatus
	ClaimedBy  int // worker ID
}

// UnsafeBroker simulates a task broker with missing synchronization.
// In a real system, database transactions provide isolation between workers.
// This struct shows what happens when that isolation is removed — the same
// concurrency bugs that exist in the in-memory layer between dequeue and ack.
type UnsafeBroker struct {
	tasks        []*BrokerTask
	activeCount  int           // RACE: unprotected — multiple workers read/write
	errorCount   int           // RACE: unprotected
	lastWorkerID int           // RACE: last worker to claim a task
	leaseTimeout time.Duration // RACE: if updated concurrently
}

func NewUnsafeBroker(n int) *UnsafeBroker {
	b := &UnsafeBroker{leaseTimeout: 30 * time.Second}
	for i := 0; i < n; i++ {
		b.tasks = append(b.tasks, &BrokerTask{
			ID: i, Type: "email", MaxRetries: 3,
			Status: StatusPending,
		})
	}
	return b
}

// Dequeue claims the next pending task — NO lock on activeCount/lastWorkerID
func (b *UnsafeBroker) Dequeue(workerID int) *BrokerTask {
	for _, t := range b.tasks {
		if t.Status == StatusPending {
			t.Status = StatusClaimed
			t.ClaimedBy = workerID
			b.activeCount++        // RACE: read-modify-write without lock
			b.lastWorkerID = workerID // RACE: write from multiple goroutines
			return t
		}
	}
	return nil
}

// Ack marks a task completed — NO lock on activeCount
func (b *UnsafeBroker) Ack(t *BrokerTask) {
	t.Status = StatusCompleted
	b.activeCount-- // RACE: paired with the increment in Dequeue
}

// Nack records a failure — NO lock on errorCount
func (b *UnsafeBroker) Nack(t *BrokerTask, workerID int) {
	t.RetryCount++
	if t.RetryCount >= t.MaxRetries {
		t.Status = StatusFailed
	} else {
		t.Status = StatusPending
	}
	b.errorCount++ // RACE: multiple workers incrementing simultaneously
}

// UpdateLeaseTimeout simulates dynamic config update — NO lock
func (b *UnsafeBroker) UpdateLeaseTimeout(d time.Duration) {
	b.leaseTimeout = d // RACE: written here, read in Dequeue simultaneously
}

// ---- Tests ----

// TestBrokerRace_WorkerPool — multiple workers concurrently dequeue/ack tasks.
// Races: activeCount (write/write), lastWorkerID (write/write)
func TestBrokerRace_WorkerPool(t *testing.T) {
	broker := NewUnsafeBroker(200)
	var wg sync.WaitGroup

	// 8 workers all hitting Dequeue/Ack simultaneously
	for w := 0; w < 8; w++ {
		workerID := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				task := broker.Dequeue(workerID)
				if task == nil {
					return
				}
				// Simulate work
				time.Sleep(time.Microsecond)
				broker.Ack(task)
			}
		}()
	}

	wg.Wait()
	t.Logf("active=%d errors=%d lastWorker=%d",
		broker.activeCount, broker.errorCount, broker.lastWorkerID)
}

// TestBrokerRace_NackCounter — workers failing tasks simultaneously.
// Race: errorCount (write/write) — same unprotected read-modify-write pattern as the counter race above
func TestBrokerRace_NackCounter(t *testing.T) {
	broker := NewUnsafeBroker(100)
	var wg sync.WaitGroup

	for w := 0; w < 5; w++ {
		workerID := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, task := range broker.tasks[:20] {
				broker.Nack(task, workerID) // all writing errorCount simultaneously
			}
		}()
	}

	wg.Wait()
}

// TestBrokerRace_DynamicConfig — lease timeout updated while workers are reading it.
// Race: leaseTimeout field (read/write) — mirrors what happens when you add
// live config reload to a running broker
func TestBrokerRace_DynamicConfig(t *testing.T) {
	broker := NewUnsafeBroker(50)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Config updater goroutine — simulates hot config reload
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				broker.UpdateLeaseTimeout(30 * time.Second) // WRITE
				broker.UpdateLeaseTimeout(60 * time.Second) // WRITE
			}
		}
	}()

	// Workers reading leaseTimeout via Dequeue simultaneously
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				_ = broker.leaseTimeout // READ — racing with updater
				time.Sleep(time.Microsecond)
			}
		}(w)
	}

	time.Sleep(5 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// ---- Safe version for comparison ----

type SafeBroker struct {
	mu           sync.Mutex
	tasks        []*BrokerTask
	activeCount  atomic.Int64
	errorCount   atomic.Int64
	lastWorkerID atomic.Int64
	leaseTimeout atomic.Int64 // store as nanoseconds
}

func NewSafeBroker(n int) *SafeBroker {
	b := &SafeBroker{}
	b.leaseTimeout.Store(int64(30 * time.Second))
	for i := 0; i < n; i++ {
		b.tasks = append(b.tasks, &BrokerTask{
			ID: i, Type: "email", MaxRetries: 3, Status: StatusPending,
		})
	}
	return b
}

func (b *SafeBroker) Dequeue(workerID int) *BrokerTask {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range b.tasks {
		if t.Status == StatusPending {
			t.Status = StatusClaimed
			t.ClaimedBy = workerID
			b.activeCount.Add(1)
			b.lastWorkerID.Store(int64(workerID))
			return t
		}
	}
	return nil
}

func (b *SafeBroker) Ack(t *BrokerTask) {
	b.mu.Lock()
	t.Status = StatusCompleted
	b.mu.Unlock()
	b.activeCount.Add(-1)
}

func (b *SafeBroker) UpdateLeaseTimeout(d time.Duration) {
	b.leaseTimeout.Store(int64(d))
}

func TestBrokerNoRace_SafeWorkerPool(t *testing.T) {
	broker := NewSafeBroker(200)
	var wg sync.WaitGroup

	for w := 0; w < 8; w++ {
		workerID := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				task := broker.Dequeue(workerID)
				if task == nil {
					return
				}
				time.Sleep(time.Microsecond)
				broker.Ack(task)
			}
		}()
	}

	// Simultaneous config update — safe because atomic
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			broker.UpdateLeaseTimeout(time.Duration(30+i) * time.Second)
		}
	}()

	wg.Wait()
	t.Logf("active=%d errors=%d lastWorker=%d",
		broker.activeCount.Load(), broker.errorCount.Load(), broker.lastWorkerID.Load())
}
