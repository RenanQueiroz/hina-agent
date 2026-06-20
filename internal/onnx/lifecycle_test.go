package onnx

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBundle is a stand-in for a loaded model set: it records that it was closed.
type fakeBundle struct{ closed *atomic.Int32 }

func (f fakeBundle) Close() error { f.closed.Add(1); return nil }

// waitFor polls cond until true or the deadline, failing the test otherwise. Used
// to avoid racing the idle-unload timer with a fixed sleep.
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", d, msg)
}

func TestLifecycleLazyLoadAndReuse(t *testing.T) {
	var loads atomic.Int32
	var closed atomic.Int32
	lc := NewLifecycle(time.Hour, func(context.Context) (fakeBundle, error) {
		loads.Add(1)
		return fakeBundle{closed: &closed}, nil
	}, Hooks{})
	defer lc.Close()

	if lc.Loaded() {
		t.Fatal("should start cold")
	}
	_, rel1, err := lc.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	_, rel2, err := lc.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want 1 (reuse)", loads.Load())
	}
	if !lc.Loaded() {
		t.Fatal("should be loaded after acquire")
	}
	// Two holders: releasing once does not unload (long TTL anyway).
	rel1()
	rel1() // idempotent: second call is a no-op
	rel2()
}

func TestLifecycleIdleUnload(t *testing.T) {
	var closed atomic.Int32
	var loaded, unloaded atomic.Int32
	lc := NewLifecycle(15*time.Millisecond, func(context.Context) (fakeBundle, error) {
		return fakeBundle{closed: &closed}, nil
	}, Hooks{
		OnLoad:   func(time.Duration) { loaded.Add(1) },
		OnUnload: func() { unloaded.Add(1) },
	})
	defer lc.Close()

	_, rel, err := lc.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	rel() // last holder gone -> idle countdown starts

	waitFor(t, time.Second, func() bool { return !lc.Loaded() }, "bundle should unload after idle TTL")
	if closed.Load() != 1 {
		t.Fatalf("closed = %d, want 1", closed.Load())
	}
	if loaded.Load() != 1 || unloaded.Load() != 1 {
		t.Fatalf("hooks: loaded=%d unloaded=%d, want 1/1", loaded.Load(), unloaded.Load())
	}

	// A fresh Acquire reloads it.
	_, rel2, err := lc.Acquire(context.Background())
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if !lc.Loaded() {
		t.Fatal("should reload on acquire")
	}
	rel2()
}

// A holder pins the bundle: the idle timer must not unload it while in use.
func TestLifecyclePinnedDuringUse(t *testing.T) {
	var closed atomic.Int32
	lc := NewLifecycle(10*time.Millisecond, func(context.Context) (fakeBundle, error) {
		return fakeBundle{closed: &closed}, nil
	}, Hooks{})
	defer lc.Close()

	_, rel, _ := lc.Acquire(context.Background())
	time.Sleep(40 * time.Millisecond) // exceed the TTL while still holding
	if !lc.Loaded() || closed.Load() != 0 {
		t.Fatalf("bundle unloaded while pinned (loaded=%v closed=%d)", lc.Loaded(), closed.Load())
	}
	rel()
	waitFor(t, time.Second, func() bool { return closed.Load() == 1 }, "should unload after release")
}

// idleTTL<=0 means keep-warm: never auto-unload.
func TestLifecycleKeepWarm(t *testing.T) {
	var closed atomic.Int32
	lc := NewLifecycle(0, func(context.Context) (fakeBundle, error) {
		return fakeBundle{closed: &closed}, nil
	}, Hooks{})
	defer lc.Close()
	_, rel, _ := lc.Acquire(context.Background())
	rel()
	time.Sleep(30 * time.Millisecond)
	if !lc.Loaded() {
		t.Fatal("keep-warm lifecycle must not auto-unload")
	}
}

// A cancellation during a (non-interruptible) load discards the freshly-loaded
// bundle rather than committing it warm + firing OnLoad for a request nobody
// wants.
func TestLifecycleCancelDuringLoadDiscards(t *testing.T) {
	var closed, loaded atomic.Int32
	loadStarted := make(chan struct{})
	lc := NewLifecycle(time.Hour, func(context.Context) (fakeBundle, error) {
		close(loadStarted)
		time.Sleep(30 * time.Millisecond) // load runs to completion (not interruptible)
		return fakeBundle{closed: &closed}, nil
	}, Hooks{OnLoad: func(time.Duration) { loaded.Add(1) }})
	defer lc.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-loadStarted; cancel() }() // cancel while the load is in flight

	if _, _, err := lc.Acquire(ctx); err == nil {
		t.Fatal("expected a cancellation error")
	}
	if lc.Loaded() {
		t.Fatal("a cancelled load must not commit the bundle")
	}
	waitFor(t, time.Second, func() bool { return closed.Load() == 1 }, "discarded bundle should be closed")
	if loaded.Load() != 0 {
		t.Fatal("OnLoad must not fire for a cancelled load")
	}
}

func TestLifecycleLoadErrorPropagates(t *testing.T) {
	boom := errors.New("boom")
	var errs atomic.Int32
	lc := NewLifecycle(time.Hour, func(context.Context) (fakeBundle, error) {
		return fakeBundle{}, boom
	}, Hooks{OnError: func(error) { errs.Add(1) }})
	defer lc.Close()

	_, _, err := lc.Acquire(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if lc.Loaded() {
		t.Fatal("failed load must leave the lifecycle cold")
	}
	if errs.Load() != 1 {
		t.Fatalf("OnError calls = %d, want 1", errs.Load())
	}
}

// Concurrent first-acquirers single-flight the load.
func TestLifecycleSingleFlight(t *testing.T) {
	var loads atomic.Int32
	var closed atomic.Int32
	lc := NewLifecycle(time.Hour, func(context.Context) (fakeBundle, error) {
		loads.Add(1)
		time.Sleep(20 * time.Millisecond) // widen the race window
		return fakeBundle{closed: &closed}, nil
	}, Hooks{})
	defer lc.Close()

	var wg sync.WaitGroup
	rels := make([]func(), 8)
	for i := range rels {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, rel, err := lc.Acquire(context.Background())
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			rels[i] = rel
		}(i)
	}
	wg.Wait()
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want 1 (single-flight)", loads.Load())
	}
	for _, r := range rels {
		if r != nil {
			r()
		}
	}
}

func TestLifecycleCloseUnloadsAndRejects(t *testing.T) {
	var closed atomic.Int32
	lc := NewLifecycle(time.Hour, func(context.Context) (fakeBundle, error) {
		return fakeBundle{closed: &closed}, nil
	}, Hooks{})

	_, rel, _ := lc.Acquire(context.Background())
	rel()
	lc.Close()
	if closed.Load() != 1 {
		t.Fatalf("closed = %d, want 1 after Close", closed.Load())
	}
	if _, _, err := lc.Acquire(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("acquire after close = %v, want ErrClosed", err)
	}
	lc.Close() // idempotent
}

// Closing while a holder is active defers the unload to the final release.
func TestLifecycleCloseDefersWhileHeld(t *testing.T) {
	var closed atomic.Int32
	lc := NewLifecycle(time.Hour, func(context.Context) (fakeBundle, error) {
		return fakeBundle{closed: &closed}, nil
	}, Hooks{})

	_, rel, _ := lc.Acquire(context.Background())
	lc.Close()
	if closed.Load() != 0 {
		t.Fatal("must not close the bundle while a holder is active")
	}
	rel()
	if closed.Load() != 1 {
		t.Fatalf("closed = %d, want 1 after final release", closed.Load())
	}
}
