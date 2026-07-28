package daemon

import (
	"sync/atomic"
	"time"
)

// heartbeat は 1 つのループ goroutine が「最後に 1 回分の作業を完了した時刻」を
// 保持する。watchdog goroutine はこれを見て、ループが長時間動いていないループが
// 無いか（＝デッドロックやハングが起きていないか）を判定する（DESIGN.md 5章
// 「ハング検知」のレイヤ 3）。
type heartbeat struct {
	unixNano atomic.Int64
}

func newHeartbeat(now time.Time) *heartbeat {
	h := &heartbeat{}
	h.mark(now)
	return h
}

// mark は t を最後の生存時刻として記録する。
func (h *heartbeat) mark(t time.Time) {
	h.unixNano.Store(t.UnixNano())
}

// since は now から最後に mark された時刻までの経過時間を返す。
func (h *heartbeat) since(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, h.unixNano.Load()))
}
