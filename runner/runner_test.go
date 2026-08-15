package runner

import (
	"context"
	"errors"
	"testing"
	"time"
)

// swap installs v at *p and returns a restore closure, so tests can override a
// package-level seam with `defer swap(&fn, stub)()`.
func swap[T any](p *T, v T) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

var errBoom = errors.New("boom")

// TestNewRetryBackoff_Config pins the migrated schedule's configuration: 5s
// initial, doubling, 60s cap, no jitter.
func TestNewRetryBackoff_Config(t *testing.T) {
	b := newRetryBackoff()
	if b.InitialInterval != 5*time.Second {
		t.Errorf("InitialInterval: got %s, want 5s", b.InitialInterval)
	}
	if b.Multiplier != 2 {
		t.Errorf("Multiplier: got %v, want 2", b.Multiplier)
	}
	if b.MaxInterval != 60*time.Second {
		t.Errorf("MaxInterval: got %s, want 60s", b.MaxInterval)
	}
	if b.RandomizationFactor != 0 {
		t.Errorf("RandomizationFactor: got %v, want 0", b.RandomizationFactor)
	}
}

// TestRunWorker_BackoffScheduleCapAndReset is the core behavioural test. It
// drives runWorker through a scripted sequence of job outcomes and asserts the
// exact wait schedule observed by the sleep seam: doubling from 5s, capped at
// 60s, then rewound to 5s after a successful iteration.
func TestRunWorker_BackoffScheduleCapAndReset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 5 failures (exercise doubling + the 60s cap), 1 success (reset), then
	// 2 more failures (prove the schedule restarts at 5s).
	outcomes := []error{
		errBoom, errBoom, errBoom, errBoom, errBoom,
		nil,
		errBoom, errBoom,
	}
	call := 0
	defer swap(&runOneJobFn, func(context.Context, int, *gh, PersistedConfig, RunOptions) error {
		if call >= len(outcomes) {
			cancel() // script exhausted: unwind the loop on the next ctx check.
			return errBoom
		}
		e := outcomes[call]
		call++
		return e
	})()

	var waits []time.Duration
	defer swap(&sleepFn, func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	})()

	runWorker(ctx, 0, nil, PersistedConfig{}, RunOptions{})

	want := []time.Duration{
		5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second,
		5 * time.Second, 10 * time.Second,
	}
	if len(waits) != len(want) {
		t.Fatalf("wait count: got %d (%v), want %d (%v)", len(waits), waits, len(want), want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("wait[%d]: got %s, want %s", i, waits[i], want[i])
		}
	}
}

// TestRunWorker_CtxAlreadyCancelled covers the top-of-loop guard: a cancelled
// ctx returns before any job runs.
func TestRunWorker_CtxAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	defer swap(&runOneJobFn, func(context.Context, int, *gh, PersistedConfig, RunOptions) error {
		called = true
		return nil
	})()

	runWorker(ctx, 0, nil, PersistedConfig{}, RunOptions{})
	if called {
		t.Error("runOneJobFn ran despite a pre-cancelled ctx")
	}
}

// TestRunWorker_CtxCancelledDuringJob covers the post-job guard: the job errors
// but ctx was cancelled meanwhile, so we unwind without waiting.
func TestRunWorker_CtxCancelledDuringJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	defer swap(&runOneJobFn, func(context.Context, int, *gh, PersistedConfig, RunOptions) error {
		cancel()
		return errBoom
	})()
	slept := false
	defer swap(&sleepFn, func(context.Context, time.Duration) error {
		slept = true
		return nil
	})()

	runWorker(ctx, 0, nil, PersistedConfig{}, RunOptions{})
	if slept {
		t.Error("slept after ctx was cancelled mid-job")
	}
}

// TestRunWorker_CancelDuringWait covers the sleep-seam abort path: the wait is
// cut short by cancellation and runWorker returns.
func TestRunWorker_CancelDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	defer swap(&runOneJobFn, func(context.Context, int, *gh, PersistedConfig, RunOptions) error {
		return errBoom
	})()
	defer swap(&sleepFn, func(context.Context, time.Duration) error {
		return context.Canceled
	})()

	done := make(chan struct{})
	go func() {
		runWorker(ctx, 0, nil, PersistedConfig{}, RunOptions{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWorker did not return after the wait was aborted")
	}
}

// TestSleepFn_Completes covers the default sleep seam's timer branch.
func TestSleepFn_Completes(t *testing.T) {
	if err := sleepFn(context.Background(), time.Millisecond); err != nil {
		t.Errorf("sleepFn returned %v, want nil on timer completion", err)
	}
}

// TestSleepFn_Cancelled covers the default sleep seam's cancellation branch.
func TestSleepFn_Cancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepFn(ctx, time.Hour); err == nil {
		t.Error("sleepFn returned nil, want ctx error on cancellation")
	}
}
