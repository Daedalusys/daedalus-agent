// Package daemon_test holds black-box tests for the daemon lifecycle.
package daemon_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Daedalusys/daedalus-agent/internal/config"
	"github.com/Daedalusys/daedalus-agent/internal/daemon"
)

// newTestConfig returns a minimal Config that the daemon skeleton accepts.
func newTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		AgentID:  "test-agent",
		DataDir:  t.TempDir(),
		AuditDir: t.TempDir(),
	}
}

// waitFor spins up to timeout waiting for pred to return true.
// It returns true if pred became true within the budget.
func waitFor(t *testing.T, timeout time.Duration, pred func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return pred()
}

func TestNew_StateIsStopped(t *testing.T) {
	d := daemon.New(newTestConfig(t))
	if got := d.State(); got != daemon.StateStopped {
		t.Fatalf("State() after New = %v, want %v", got, daemon.StateStopped)
	}
}

func TestStartStop_StateTransitions(t *testing.T) {
	d := daemon.New(newTestConfig(t))

	runDone := make(chan error, 1)
	go func() { runDone <- d.Start(context.Background()) }()

	if !waitFor(t, 2*time.Second, func() bool { return d.State() == daemon.StateRunning }) {
		t.Fatal("daemon did not reach StateRunning within 2s")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Start returned err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after Stop")
	}

	if got := d.State(); got != daemon.StateStopped {
		t.Fatalf("final state = %v, want %v", got, daemon.StateStopped)
	}
}

func TestStart_WhileRunning_ReturnsErrAlreadyStarted(t *testing.T) {
	d := daemon.New(newTestConfig(t))

	runDone := make(chan error, 1)
	go func() { runDone <- d.Start(context.Background()) }()

	if !waitFor(t, 2*time.Second, func() bool { return d.State() == daemon.StateRunning }) {
		t.Fatal("daemon did not reach StateRunning within 2s")
	}

	if err := d.Start(context.Background()); !errors.Is(err, daemon.ErrAlreadyStarted) {
		t.Fatalf("second Start: err = %v, want ErrAlreadyStarted", err)
	}

	// Clean up so the goroutine returns.
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("Stop during cleanup: %v", err)
	}
	if err := <-runDone; err != nil {
		t.Errorf("Start returned unexpected err: %v", err)
	}
}

func TestStop_WhileStopped_ReturnsErrNotRunning(t *testing.T) {
	d := daemon.New(newTestConfig(t))
	if err := d.Stop(context.Background()); !errors.Is(err, daemon.ErrNotRunning) {
		t.Fatalf("Stop on stopped daemon: err = %v, want ErrNotRunning", err)
	}
}

func TestStart_ContextCancel_StopsDaemon(t *testing.T) {
	d := daemon.New(newTestConfig(t))

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Start(ctx) }()

	if !waitFor(t, 2*time.Second, func() bool { return d.State() == daemon.StateRunning }) {
		t.Fatal("daemon did not reach StateRunning within 2s")
	}

	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Start returned err after ctx cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after ctx cancel")
	}

	if got := d.State(); got != daemon.StateStopped {
		t.Fatalf("state after ctx cancel = %v, want %v", got, daemon.StateStopped)
	}
}

// pastDeadlineCtx 返回一个**构造时即已过期**的 context。
//
// 为什么不用 WithTimeout(1ms) + Sleep:那个写法把"期限已过"寄托在 runtime
// 定时器的回调时刻上 —— 墙钟过了 2ms,定时器回调可能还没跑,ctx.Err() 仍然是
// nil;此时 Stop 若恰好命中 select 的 <-done 分支就返回 nil(daedalus-agent#31:
// 200 轮里约 1 次命中这个窗口,全量 go test 因此随机红)。
//
// context.WithDeadline 对**已经过去的**期限是同步 cancel 的(Err() 立刻返回
// context.DeadlineExceeded,不等定时器),用它表达"期限已过"没有这个时间窗。
func pastDeadlineCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
}

func TestStop_StopContextDeadline_ReturnsError(t *testing.T) {
	d := daemon.New(newTestConfig(t))

	// Start in a goroutine but never call Stop nor cancel the parent context.
	// We expect a Stop with a near-zero deadline to return context.DeadlineExceeded.
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()

	runDone := make(chan error, 1)
	go func() { runDone <- d.Start(runCtx) }()

	if !waitFor(t, 2*time.Second, func() bool { return d.State() == daemon.StateRunning }) {
		t.Fatal("daemon did not reach StateRunning within 2s")
	}

	deadlineCtx, cancelDeadline := pastDeadlineCtx(t)
	defer cancelDeadline()
	// 前提自证:构造出的 ctx 必须**此刻**就是过期态。若不是,下面的失败就不是
	// Stop 的问题,而是前提没立住 —— 直接说出来,别让测试把两件事混为一谈。
	if !errors.Is(deadlineCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("前提不成立:构造出的 ctx 未过期,err = %v", deadlineCtx.Err())
	}

	if err := d.Stop(deadlineCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop with past deadline: err = %v, want DeadlineExceeded", err)
	}

	// Clean up: cancel parent context and wait for Start to return.
	cancelRun()
	if err := <-runDone; err != nil {
		t.Errorf("Start returned unexpected err: %v", err)
	}
}

// TestStop_StopContextDeadline_NeverReportsSuccess 是上面那条的加固版:同一个
// 场景连跑 200 次,每次都必须报 DeadlineExceeded。旧实现的 select 在两个分支
// 同时就绪时随机挑一个,本机慢(关机流程比 select 长)基本复现不出,只有 CI 的
// 快机器才偶发红 —— 所以这条测试的作用是"在快机器上把随机性压到可察觉"。
func TestStop_StopContextDeadline_NeverReportsSuccess(t *testing.T) {
	// 前提自证:pastDeadlineCtx 必须**同步**给出过期态,否则这条测试量的
	// 就不是"Stop 会不会把超时报成成功",而是"定时器有没有跑到"。
	if probe, cancelProbe := pastDeadlineCtx(t); true {
		if !errors.Is(probe.Err(), context.DeadlineExceeded) {
			cancelProbe()
			t.Fatalf("前提不成立:pastDeadlineCtx 构造后即非过期,err = %v", probe.Err())
		}
		cancelProbe()
	}
	for i := 0; i < 200; i++ {
		d := daemon.New(newTestConfig(t))
		runCtx, cancelRun := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- d.Start(runCtx) }()
		if !waitFor(t, 2*time.Second, func() bool { return d.State() == daemon.StateRunning }) {
			cancelRun()
			t.Fatalf("第 %d 轮:daemon 未在 2s 内进入 Running", i)
		}
		deadlineCtx, cancelDeadline := pastDeadlineCtx(t)
		err := d.Stop(deadlineCtx)
		cancelDeadline()
		if !errors.Is(err, context.DeadlineExceeded) {
			cancelRun()
			t.Fatalf("第 %d 轮:Stop 过期 deadline 返回 %v, want DeadlineExceeded", i, err)
		}
		cancelRun()
		if err := <-runDone; err != nil {
			t.Fatalf("第 %d 轮:Start 返回意外错误 %v", i, err)
		}
	}
}

func TestStartStop_IsConcurrencySafe(t *testing.T) {
	// Hammer Start+Stop from multiple goroutines, each on its own
	// freshly-constructed Daemon (the daemon is single-use). The state
	// machine must not race and Stop must succeed cleanly each cycle.
	//
	// waitRunning polls the state until the daemon reaches StateRunning
	// or the budget elapses. It does NOT use *testing.T because it is
	// called from worker goroutines where t.Fatalf is unsafe.
	waitRunning := func(d *daemon.Daemon, budget time.Duration) bool {
		deadline := time.Now().Add(budget)
		for time.Now().Before(deadline) {
			if d.State() == daemon.StateRunning {
				return true
			}
			time.Sleep(time.Millisecond)
		}
		return d.State() == daemon.StateRunning
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				d := daemon.New(newTestConfig(t))

				runDone := make(chan error, 1)
				go func() { runDone <- d.Start(context.Background()) }()

				waitRunning(d, 100*time.Millisecond)

				if err := d.Stop(context.Background()); err != nil {
					t.Errorf("Stop: unexpected err %v", err)
				}
				if err := <-runDone; err != nil {
					t.Errorf("Start returned unexpected err: %v", err)
				}
			}
		}()
	}
	wg.Wait()
}
