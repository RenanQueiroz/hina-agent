package onnx

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// ErrClosed is returned by Acquire after the Lifecycle has been shut down.
var ErrClosed = errors.New("onnx: lifecycle closed")

// Hooks observe a Lifecycle's load/unload transitions so the owner can surface
// them as events (RuntimeModelLoaded/Unloaded/Error) and latency metrics. All
// are optional and are called outside the internal lock.
type Hooks struct {
	OnLoad   func(d time.Duration) // a cold load completed; d is the load duration
	OnUnload func()                // the bundle was unloaded after the idle TTL
	OnError  func(err error)       // a load attempt failed
}

// Lifecycle lazily loads an expensive model bundle T on first Acquire and
// unloads it after idleTTL with no active holders. It is the shared idle-unload
// manager the plan calls for (TTS now, ASR in Phase 5): model-agnostic over any
// io.Closer bundle, with reference counting so an in-flight inference is never
// unloaded mid-use. Loads are single-flight — concurrent first-Acquire callers
// wait on one load rather than each loading their own copy.
//
// A non-positive idleTTL means "never auto-unload" (stay warm until Close).
type Lifecycle[T io.Closer] struct {
	idleTTL time.Duration
	load    func(context.Context) (T, error)
	hooks   Hooks

	mu      sync.Mutex
	val     T
	loaded  bool
	refs    int
	timer   *time.Timer
	loading chan struct{} // non-nil while a load is in flight (single-flight gate)
	closed  bool
}

// NewLifecycle builds a Lifecycle that loads T via load and unloads it after
// idleTTL idle. load receives the Acquire caller's context so a slow cold load
// is cancellable.
func NewLifecycle[T io.Closer](idleTTL time.Duration, load func(context.Context) (T, error), hooks Hooks) *Lifecycle[T] {
	return &Lifecycle[T]{idleTTL: idleTTL, load: load, hooks: hooks}
}

// Acquire returns the loaded bundle, loading it first if cold, and a release
// function that must be called exactly once when the caller is done with it.
// While at least one holder is outstanding the bundle is pinned (the idle timer
// is disarmed); the idle countdown (re)starts when the last holder releases.
// release is idempotent.
func (l *Lifecycle[T]) Acquire(ctx context.Context) (T, func(), error) {
	var zero T
	for {
		l.mu.Lock()
		switch {
		case l.closed:
			l.mu.Unlock()
			return zero, nil, ErrClosed
		case l.loaded:
			l.refs++
			l.stopTimerLocked()
			v := l.val
			l.mu.Unlock()
			return v, l.releaseFn(), nil
		case l.loading != nil:
			// Another goroutine is loading; wait for it, then retry the switch.
			ch := l.loading
			l.mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return zero, nil, ctx.Err()
			}
			continue
		}
		// This goroutine owns the load.
		ch := make(chan struct{})
		l.loading = ch
		l.mu.Unlock()

		start := time.Now()
		v, err := l.load(ctx)

		l.mu.Lock()
		l.loading = nil
		close(ch) // wake any waiters so they re-evaluate
		switch {
		case err != nil:
			l.mu.Unlock()
			if l.hooks.OnError != nil {
				l.hooks.OnError(err)
			}
			return zero, nil, err
		case l.closed:
			// Closed while we were loading: discard the fresh bundle.
			l.mu.Unlock()
			_ = v.Close()
			return zero, nil, ErrClosed
		case ctx.Err() != nil:
			// The caller cancelled during (or just after) the load — a single ORT Open
			// isn't interruptible, so the load can complete for a request nobody wants.
			// Discard the bundle rather than committing it warm + firing OnLoad; a
			// waiting acquirer re-loads (loading was cleared above).
			l.mu.Unlock()
			_ = v.Close()
			return zero, nil, ctx.Err()
		}
		l.val = v
		l.loaded = true
		l.refs++
		l.mu.Unlock()
		if l.hooks.OnLoad != nil {
			l.hooks.OnLoad(time.Since(start))
		}
		return v, l.releaseFn(), nil
	}
}

// releaseFn returns a single-use release closure for one Acquire.
func (l *Lifecycle[T]) releaseFn() func() {
	var once sync.Once
	return func() { once.Do(l.release) }
}

func (l *Lifecycle[T]) release() {
	l.mu.Lock()
	if l.refs > 0 {
		l.refs--
	}
	if l.refs != 0 {
		l.mu.Unlock()
		return
	}
	// Last holder released.
	if l.closed && l.loaded {
		// Close was deferred while we were in use; do it now.
		v := l.val
		var zero T
		l.val, l.loaded = zero, false
		l.mu.Unlock()
		_ = v.Close()
		return
	}
	if l.loaded {
		l.armTimerLocked()
	}
	l.mu.Unlock()
}

func (l *Lifecycle[T]) armTimerLocked() {
	if l.idleTTL <= 0 {
		return // keep warm forever
	}
	l.stopTimerLocked()
	l.timer = time.AfterFunc(l.idleTTL, l.idleUnload)
}

func (l *Lifecycle[T]) stopTimerLocked() {
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
}

// idleUnload is the idle-timer callback: unload only if still idle (no holders)
// and not already closed/unloaded.
func (l *Lifecycle[T]) idleUnload() {
	l.mu.Lock()
	if l.closed || !l.loaded || l.refs != 0 {
		l.mu.Unlock()
		return
	}
	v := l.val
	var zero T
	l.val, l.loaded, l.timer = zero, false, nil
	l.mu.Unlock()
	_ = v.Close()
	if l.hooks.OnUnload != nil {
		l.hooks.OnUnload()
	}
}

// Loaded reports whether the bundle is currently resident.
func (l *Lifecycle[T]) Loaded() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loaded
}

// Close shuts the Lifecycle down: no further Acquire succeeds, and the bundle is
// unloaded once the last in-flight holder releases (immediately if idle). Safe to
// call more than once.
func (l *Lifecycle[T]) Close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	l.stopTimerLocked()
	if l.refs == 0 && l.loaded {
		v := l.val
		var zero T
		l.val, l.loaded = zero, false
		l.mu.Unlock()
		_ = v.Close()
		return
	}
	l.mu.Unlock()
}
