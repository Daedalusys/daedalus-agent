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
)

// ShutdownTimeout bounds the graceful shutdown window. Once a shutdown
// is initiated (via Stop or context cancellation), the daemon has this
// long to drain in-flight work before returning.
const ShutdownTimeout = 30 * time.Second

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

// gracefulShutdown runs the shutdown hooks. T1 is a no-op because there
// is no real work to drain; T3 will hook in here for mTLS connection
// draining, T5 for in-flight RPC draining.
//
// The ctx is bounded by ShutdownTimeout (30s); shutdown hooks should
// honour it. Errors are returned to the Start caller.
func (d *Daemon) gracefulShutdown(ctx context.Context) error {
	// Skeleton: nothing to drain. T3+ will register hooks here.
	_ = ctx
	return nil
}
