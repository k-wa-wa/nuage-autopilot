package sdnotify

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestNotify_NoSocketConfigured(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")

	ok, err := Notify("READY=1")
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want false when NOTIFY_SOCKET is unset")
	}
}

func TestNotify_SendsToSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "notify.sock")

	addr, err := net.ResolveUnixAddr("unixgram", sockPath)
	if err != nil {
		t.Fatalf("ResolveUnixAddr: %v", err)
	}
	listener, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatalf("ListenUnixgram: %v", err)
	}
	defer listener.Close()

	t.Setenv("NOTIFY_SOCKET", sockPath)

	ok, err := Notify("READY=1")
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true")
	}

	buf := make([]byte, 64)
	if err := listener.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	n, err := listener.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := string(buf[:n]); got != "READY=1" {
		t.Fatalf("received %q, want %q", got, "READY=1")
	}
}

func TestWatchdogInterval(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "")
	if _, ok := WatchdogInterval(); ok {
		t.Fatalf("ok = true, want false when WATCHDOG_USEC is unset")
	}

	t.Setenv("WATCHDOG_USEC", "not-a-number")
	if _, ok := WatchdogInterval(); ok {
		t.Fatalf("ok = true, want false for an invalid WATCHDOG_USEC")
	}

	// 120s に相当する 120000000 マイクロ秒 → 推奨間隔はその半分の 60s。
	t.Setenv("WATCHDOG_USEC", "120000000")
	got, ok := WatchdogInterval()
	if !ok {
		t.Fatalf("ok = false, want true for a valid WATCHDOG_USEC")
	}
	if want := 60 * time.Second; got != want {
		t.Fatalf("WatchdogInterval = %v, want %v", got, want)
	}
}
