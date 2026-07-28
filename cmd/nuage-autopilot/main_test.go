package main

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestRun_VersionFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"--version"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(--version) exit code = %d, want 0", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != version {
		t.Fatalf("stdout = %q, want %q", got, version)
	}
}

func TestRun_MissingRepo(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run(nil, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("run(nil) exit code = %d, want 2", code)
	}
	if stderr.Len() == 0 {
		t.Fatalf("stderr is empty, want error message about missing --repos")
	}
}

// TestRun_StartsDaemonAndStopsOnSIGTERM は run() が daemon.Run へ正しく配線され、
// SIGTERM を受けて正常終了することを確認する。
//
// run() は内部で signal.NotifyContext(os.Interrupt, syscall.SIGTERM) を使って
// 常駐する（Phase 1 以降の設計。oneshot だった旧実装と異なり、1 サイクルで
// 戻ってくることはない）。テストプロセス自身に SIGTERM を送ることで終了させる。
func TestRun_StartsDaemonAndStopsOnSIGTERM(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("NUAGE_STATE_DIR", stateDir)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_AUTHOR_EMAIL", "")

	var stdout, stderr bytes.Buffer
	var mu sync.Mutex // stdout/stderr への書き込みと読み取りを race 検出器から守る

	code := make(chan int, 1)
	go func() {
		mu.Lock()
		out, errw := &stdout, &stderr
		mu.Unlock()
		c := run([]string{"--repos", "k-wa-wa/pechka"}, syncWriter{&mu, out}, syncWriter{&mu, errw})
		code <- c
	}()

	// daemon.Run が起動し、signal.NotifyContext の登録が完了するまでの猶予。
	// store.Open は SQLite を WebAssembly として初回コンパイルするため、CPU 負荷が
	// 高い環境では時間がかかることがあり、余裕を持たせている。
	deadline := time.After(15 * time.Second)
	for {
		mu.Lock()
		started := strings.Contains(stdout.String(), "nuage-autopilot starting")
		mu.Unlock()
		if started {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("daemon did not log a startup message in time")
		case <-time.After(5 * time.Millisecond):
		}
	}

	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Signal(SIGTERM): %v", err)
	}

	select {
	case got := <-code:
		if got != 0 {
			mu.Lock()
			t.Fatalf("run() exit code = %d, want 0 (stderr: %s)", got, stderr.String())
			mu.Unlock()
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("run() did not return after SIGTERM")
	}

	mu.Lock()
	out := stdout.String()
	mu.Unlock()
	for _, want := range []string{"k-wa-wa/pechka", stateDir, "shutdown signal received"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want it to contain %q", out, want)
		}
	}
}

// syncWriter は複数 goroutine から安全に書き込める io.Writer である。
// テストのメイン goroutine が stdout/stderr のバッファを読みながら、
// run() を実行する別 goroutine が同時に書き込むため必要になる。
type syncWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (s syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
