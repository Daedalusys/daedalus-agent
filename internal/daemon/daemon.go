// Package daemon owns the lifecycle of the daedalus-agent process.
//
// T1 ships the lifecycle machinery only: state transitions, signal-aware
// shutdown, and a 30s graceful timeout. The gRPC server, mTLS wiring, and
// RPC handlers are grafted on by later tasks (T3 / T5). Anything beyond
// "shell + state machine" belongs to those tasks.
//
// A Daemon is single-use: once Stop completes, the instance is done and
// must be discarded. Process restart is the supervisor's job (systemd,
// supervisord, etc.), not the daemon's. This avoids the channel-reuse
// race that would otherwise complicate Start→Stop→Start.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Daedalusys/daedalus-agent/internal/config"
)

// Sentinel errors returned by Start and Stop.
var (
	// ErrAlreadyStarted is returned when Start is called while the
	// daemon is in StateRunning or after it has been used.
	ErrAlreadyStarted = errors.New("daemon: already started")

	// ErrNotRunning is returned when Stop is called while the daemon
	// is in StateStopped (or after it has shut down).
	ErrNotRunning = errors.New("daemon: not running")

	// ErrHookAlreadyStarted is returned by RegisterShutdownHook
	// when called after the daemon has begun running (or
	// completed a Start). Hooks must be installed before the
	// run loop begins, mirroring the "construct → start" idiom.
	ErrHookAlreadyStarted = errors.New("daemon: hooks may only be registered before Start")
)

// ShutdownTimeout bounds the graceful shutdown window. Once a shutdown
// is initiated (via Stop or context cancellation), the daemon has this
// long to drain in-flight work before returning.
const ShutdownTimeout = 30 * time.Second

// ShutdownHook is the contract for code that wants to participate
// in graceful daemon shutdown.
//
// The ctx passed to a hook is bounded by ShutdownTimeout (30s); a
// well-behaved hook watches ctx.Done() to bound its own work. Errors
// are surfaced to Start()'s return value; subsequent hooks still
// run — the daemon runs ALL hooks even if one fails, so a stuck
// cert writer cannot prevent the gRPC server from closing.
//
// Hooks are called in REVERSE registration order (LIFO). This
// mirrors the standard "teardown unwinds init" pattern: the most
// recently added resource (e.g. the gRPC server added right before
// daemon.Start) gets drained first, freeing the resource it
// depended on next (e.g. the cert loader).
type ShutdownHook func(ctx context.Context) error

// State represents the lifecycle state of the daemon.
type State int32

const (
	// StateStopped is the initial state and the state the daemon
	// returns to after a successful shutdown.
	StateStopped State = iota

	// StateRunning means Start has succeeded and the run loop is
	// executing. The daemon blocks in Start until shutdown.
	StateRunning
)

// String renders a State for log lines and CLI output.
func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateRunning:
		return "running"
	default:
		return "unknown"
	}
}

// Daemon is the lifecycle owner for the daedalus-agent process.
//
// The zero value is not usable; construct one via New. A Daemon is
// single-use: a successful Start → Stop cycle uses up the instance.
// Create a new Daemon to run again.
type Daemon struct {
	cfg *config.Config

	// mu guards all mutable fields below.
	mu sync.Mutex

	// state is the current lifecycle state.
	state State

	// stopCh is closed exactly once by Stop (via stopOnce) to signal
	// the run loop to exit.
	stopCh chan struct{}

	// done is closed by finishShutdown when the run loop has fully
	// exited. Stop waits on this.
	done chan struct{}

	// stopOnce guards the close(stopCh) call so multiple concurrent
	// Stop calls do not panic.
	stopOnce sync.Once

	// finished is true once finishShutdown has run. After this the
	// Daemon is dead and Start will refuse with ErrAlreadyStarted.
	finished bool

	// shutdownHooks are the registered ShutdownHook funcs. Run in
	// LIFO order during gracefulShutdown. Mutable only via
	// RegisterShutdownHook (which itself requires the daemon be
	// in StateStopped).
	shutdownHooks []ShutdownHook
}

// New constructs a Daemon in the StateStopped state, ready for Start.
//
// It is safe to call New concurrently; each call returns an independent
// Daemon. The cfg pointer is retained by reference; callers should not
// mutate it after passing it to New.
func New(cfg *config.Config) *Daemon {
	return &Daemon{
		cfg:    cfg,
		state:  StateStopped,
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// State returns the current lifecycle state.
//
// It is safe to call from any goroutine at any time.
func (d *Daemon) State() State {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

// RegisterShutdownHook installs hook to run during graceful
// shutdown. Hooks execute in LIFO order (the most recently
// registered runs first), so callers should register them in the
// same order they would construct dependencies: construction
// goes root → leaf, teardown goes leaf → root.
//
// Calling RegisterShutdownHook after the daemon has begun Start
// returns ErrHookAlreadyStarted; hooks can only be installed
// before the run loop is entered. This is a guard against race
// conditions where a hook could fire mid-shutdown while another
// goroutine is mid-Register.
//
// Returns nil on success. (ErrHookAlreadyStarted is the only
// error path; documented explicitly so callers can errors.Is.)
func (d *Daemon) RegisterShutdownHook(hook ShutdownHook) error {
	if hook == nil {
		return errors.New("daemon: nil hook")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state != StateStopped {
		return ErrHookAlreadyStarted
	}
	d.shutdownHooks = append(d.shutdownHooks, hook)
	return nil
}

// Start runs the daemon's main loop.
//
// It blocks until ctx is cancelled or Stop is called, then performs a
// graceful shutdown bounded by ShutdownTimeout (30s) and returns.
//
// If the daemon is already running, or has already been used, Start
// returns ErrAlreadyStarted without blocking.
func (d *Daemon) Start(ctx context.Context) error {
	d.mu.Lock()
	if d.state != StateStopped || d.finished {
		d.mu.Unlock()
		return ErrAlreadyStarted
	}
	d.state = StateRunning
	stopCh := d.stopCh
	d.mu.Unlock()

	defer d.finishShutdown()

	select {
	case <-ctx.Done():
	case <-stopCh:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	return d.gracefulShutdown(shutdownCtx)
}

// Stop requests graceful shutdown and waits up to ctx for the daemon
// to reach StateStopped.
//
// If the daemon is not running, Stop returns ErrNotRunning. If the
// daemon shuts down within ctx's deadline, Stop returns nil. Otherwise
// it returns ctx.Err() — but the daemon continues shutting down in the
// background.
//
// Stop is safe to call concurrently; only the first call closes
// stopCh (via stopOnce).
func (d *Daemon) Stop(ctx context.Context) error {
	d.mu.Lock()
	if d.state != StateRunning {
		d.mu.Unlock()
		return ErrNotRunning
	}
	stopOnce := &d.stopOnce
	stopCh := d.stopCh
	done := d.done
	d.mu.Unlock()

	stopOnce.Do(func() { close(stopCh) })

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// finishShutdown is deferred from Start. It flips the state to
// StateStopped, marks the daemon as finished (refusing further
// Starts), and signals anyone waiting in Stop.
func (d *Daemon) finishShutdown() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state = StateStopped
	d.finished = true
	close(d.done)
}

// gracefulShutdown runs the registered shutdown hooks in LIFO order.
// All hooks run regardless of individual errors so a single failing
// hook cannot leave later hooks orphaned. Errors are aggregated and
// the first non-nil is returned; subsequent errors are joined.
//
// The ctx is bounded by ShutdownTimeout (30s); hooks should honour it.
func (d *Daemon) gracefulShutdown(ctx context.Context) error {
	d.mu.Lock()
	hooks := make([]ShutdownHook, len(d.shutdownHooks))
	copy(hooks, d.shutdownHooks)
	d.mu.Unlock()

	var errs []error
	for i := len(hooks) - 1; i >= 0; i-- {
		if err := hooks[i](ctx); err != nil {
			errs = append(errs, fmt.Errorf("hook[%d]: %w", i, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
