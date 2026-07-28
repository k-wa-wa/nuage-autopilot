// Package sdnotify は systemd の sd_notify プロトコル（$NOTIFY_SOCKET への
// unixgram 送信）を実装する。cgo の libsystemd には依存しない。
//
// DESIGN.md 5章「単一プロセスにする理由」を参照。Type=notify + WatchdogSec を使うため、
// プロセスは起動完了時に READY=1 を、以後は生存を確認できている間だけ WATCHDOG=1 を
// 送る必要がある。
package sdnotify

import (
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Notify は state（例: "READY=1"）を $NOTIFY_SOCKET へ送る。
//
// $NOTIFY_SOCKET が未設定の場合（systemd 配下で動いていない場合。ローカル開発や
// テストではこれが通常である）は何もせず ok=false, err=nil を返す。呼び出し側は
// これをエラーとして扱う必要はない。
func Notify(state string) (ok bool, err error) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return false, nil
	}

	// "@" 始まりは Linux の abstract namespace socket を表し、実際のアドレスは
	// 先頭が NUL バイトになる。systemd はこの形式で NOTIFY_SOCKET を渡すことがある。
	if strings.HasPrefix(addr, "@") {
		addr = "\x00" + addr[1:]
	}

	conn, err := net.Dial("unixgram", addr)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(state)); err != nil {
		return false, err
	}
	return true, nil
}

// Ready は起動完了を通知する。
func Ready() (bool, error) { return Notify("READY=1") }

// Watchdog は生存を通知する。WatchdogSec 未満の間隔で定期的に呼ぶ必要がある。
func Watchdog() (bool, error) { return Notify("WATCHDOG=1") }

// Stopping は終了処理を開始したことを通知する。
func Stopping() (bool, error) { return Notify("STOPPING=1") }

// Status は journalctl 等に表示される任意のステータス文字列を通知する。
func Status(msg string) (bool, error) { return Notify("STATUS=" + msg) }

// WatchdogInterval は $WATCHDOG_USEC（systemd が WatchdogSec= から設定する）を
// 読み取り、その半分の間隔を推奨値として返す。systemd のドキュメントが推奨する
// 「WatchdogSec の半分以下の間隔で通知する」という指針に従う。
//
// $WATCHDOG_USEC が未設定または不正な場合は ok=false を返す。呼び出し側は
// この場合の既定値を自分で決めてよい。
func WatchdogInterval() (interval time.Duration, ok bool) {
	v := os.Getenv("WATCHDOG_USEC")
	if v == "" {
		return 0, false
	}
	usec, err := strconv.ParseInt(v, 10, 64)
	if err != nil || usec <= 0 {
		return 0, false
	}
	return time.Duration(usec) * time.Microsecond / 2, true
}
