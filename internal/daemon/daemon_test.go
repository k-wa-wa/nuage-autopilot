package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRun_RequiresAllHooks(t *testing.T) {
	if err := Run(context.Background(), Config{}); err == nil {
		t.Fatalf("Run with no hooks configured should error")
	}
}

func TestRun_CallsEachLoopImmediatelyAndStopsOnCancel(t *testing.T) {
	var pollCalls, workCalls, resyncCalls atomic.Int32

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Poller:           PollerFunc(func(ctx context.Context) (int, error) { pollCalls.Add(1); return 0, nil }),
			Worker:           WorkerFunc(func(ctx context.Context) (bool, error) { workCalls.Add(1); return false, nil }),
			Resyncer:         ResyncerFunc(func(ctx context.Context) error { resyncCalls.Add(1); return nil }),
			PollInterval:     10 * time.Millisecond,
			WorkInterval:     10 * time.Millisecond,
			ResyncInterval:   10 * time.Millisecond,
			WatchdogInterval: 10 * time.Millisecond,
		})
	}()

	deadline := time.After(2 * time.Second)
	for pollCalls.Load() == 0 || workCalls.Load() == 0 || resyncCalls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for all loops to run at least once: poll=%d work=%d resync=%d",
				pollCalls.Load(), workCalls.Load(), resyncCalls.Load())
		case <-time.After(time.Millisecond):
		}
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after context cancellation")
	}
}

// worker が「processed=true の間はバックログを drain し続ける」ことを検証する。
// WorkInterval をわざと極端に長くし、interval 待ちに律速されていたら間に合わない
// 状況を作る。
func TestRun_WorkerDrainsBacklogWithoutWaitingForInterval(t *testing.T) {
	var remaining atomic.Int32
	remaining.Store(5)
	var processedCount atomic.Int32

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Poller: PollerFunc(func(ctx context.Context) (int, error) { return 0, nil }),
			Worker: WorkerFunc(func(ctx context.Context) (bool, error) {
				if remaining.Load() <= 0 {
					return false, nil
				}
				remaining.Add(-1)
				processedCount.Add(1)
				return true, nil
			}),
			Resyncer:         ResyncerFunc(func(ctx context.Context) error { return nil }),
			PollInterval:     time.Hour,
			WorkInterval:     time.Hour,
			ResyncInterval:   time.Hour,
			WatchdogInterval: time.Hour,
		})
	}()

	deadline := time.After(2 * time.Second)
	for processedCount.Load() < 5 {
		select {
		case <-deadline:
			t.Fatalf("timed out draining backlog: processed=%d (want 5; WorkInterval=1h means it must not be waiting on the ticker)", processedCount.Load())
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after cancellation")
	}
}

// poller が enqueue した直後、worker が WorkInterval を待たずに wake 通知で
// 起こされることを検証する。
func TestRun_PollerWakesWorkerImmediately(t *testing.T) {
	var enqueuedOnce atomic.Bool
	var processedOnce atomic.Bool
	processedAt := make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Poller: PollerFunc(func(ctx context.Context) (int, error) {
				if enqueuedOnce.CompareAndSwap(false, true) {
					return 1, nil
				}
				return 0, nil
			}),
			Worker: WorkerFunc(func(ctx context.Context) (bool, error) {
				if enqueuedOnce.Load() && processedOnce.CompareAndSwap(false, true) {
					select {
					case processedAt <- struct{}{}:
					default:
					}
					return true, nil
				}
				return false, nil
			}),
			Resyncer:         ResyncerFunc(func(ctx context.Context) error { return nil }),
			PollInterval:     20 * time.Millisecond,
			WorkInterval:     time.Hour, // wake が効いていなければ絶対に間に合わない
			ResyncInterval:   time.Hour,
			WatchdogInterval: time.Hour,
		})
	}()

	select {
	case <-processedAt:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("worker was not woken by the poller's enqueue signal")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after cancellation")
	}
}

func TestRun_SendsWatchdogWhenAllLoopsAreHealthy(t *testing.T) {
	listener, sockPath := newNotifySocket(t)
	log := collectDatagrams(t, listener)
	t.Setenv("NOTIFY_SOCKET", sockPath)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	if err := Run(ctx, Config{
		Poller:           PollerFunc(func(ctx context.Context) (int, error) { return 0, nil }),
		Worker:           WorkerFunc(func(ctx context.Context) (bool, error) { return false, nil }),
		Resyncer:         ResyncerFunc(func(ctx context.Context) error { return nil }),
		PollInterval:     20 * time.Millisecond,
		WorkInterval:     20 * time.Millisecond,
		ResyncInterval:   20 * time.Millisecond,
		WatchdogInterval: 20 * time.Millisecond,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	waitForMsg(t, log, "READY=1")
	waitForMsg(t, log, "WATCHDOG=1")
	waitForMsg(t, log, "STOPPING=1")
}

func TestRun_WithholdsWatchdogWhenWorkerStalls(t *testing.T) {
	listener, sockPath := newNotifySocket(t)
	log := collectDatagrams(t, listener)
	t.Setenv("NOTIFY_SOCKET", sockPath)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	// ctx が閉じるまで戻らない worker。ハングを模している。
	blockingWorker := WorkerFunc(func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	})

	if err := Run(ctx, Config{
		Poller:           PollerFunc(func(ctx context.Context) (int, error) { return 0, nil }),
		Worker:           blockingWorker,
		Resyncer:         ResyncerFunc(func(ctx context.Context) error { return nil }),
		PollInterval:     20 * time.Millisecond,
		WorkInterval:     time.Hour,
		ResyncInterval:   20 * time.Millisecond,
		WatchdogInterval: 20 * time.Millisecond,
		WorkStaleAfter:   10 * time.Millisecond,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	waitForMsg(t, log, "READY=1")

	time.Sleep(50 * time.Millisecond)
	if got := log.snapshot(); containsMsg(got, "WATCHDOG=1") {
		t.Fatalf("did not expect WATCHDOG=1 while the worker loop is stalled, got %v", got)
	}
}

func newNotifySocket(t *testing.T) (*net.UnixConn, string) {
	t.Helper()

	// unix ソケットのパス長には OS 上限がある（macOS の sun_path は 104 バイト）。
	// t.TempDir() はテスト名をパスに含めるため、テスト名が長いとここで超過しうる。
	// テスト名に依存しない短い一時ディレクトリを別途作る。
	dir, err := os.MkdirTemp("", "nuage-autopilot-sock")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "n.sock")
	addr, err := net.ResolveUnixAddr("unixgram", sockPath)
	if err != nil {
		t.Fatalf("ResolveUnixAddr: %v", err)
	}
	listener, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatalf("ListenUnixgram: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener, sockPath
}

type datagramLog struct {
	mu   sync.Mutex
	msgs []string
}

func (d *datagramLog) add(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.msgs = append(d.msgs, s)
}

func (d *datagramLog) snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.msgs))
	copy(out, d.msgs)
	return out
}

func collectDatagrams(t *testing.T, listener *net.UnixConn) *datagramLog {
	t.Helper()
	log := &datagramLog{}
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := listener.Read(buf)
			if err != nil {
				return
			}
			log.add(string(buf[:n]))
		}
	}()
	return log
}

func containsMsg(msgs []string, want string) bool {
	for _, m := range msgs {
		if m == want {
			return true
		}
	}
	return false
}

func waitForMsg(t *testing.T, log *datagramLog, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if containsMsg(log.snapshot(), want) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %q, got %v", want, log.snapshot())
		case <-time.After(5 * time.Millisecond):
		}
	}
}
