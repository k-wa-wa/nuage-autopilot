// Package report は worker (claude) と nuage-autopilot (Go) の間の結果受け渡し
// プロトコルを持つ。
//
// worker は結果コメントを自身で GitHub に投稿しない。代わりに、環境変数
// NUAGE_REPORT_FILE が指すパスに Result を JSON で書き出すだけでよい。
// コメントの投稿・ラベル操作は Go 側（internal/cycle/executor.go）が Result を読み、
// 状態行付きコメントとして組み立てて行う。
//
// これにより状態行のフォーマットが機械保証され、worker が無言終了した場合も
// Go がフォールバックの status=blocked を確実に残せる。
package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Worker* は状態行の worker= に入る値であり、worker の識別子そのものである。
const (
	WorkerWork   = "work"
	WorkerVerify = "verify"
)

// Status* は状態行の status= に入る値である。
const (
	// StatusDone は work が担当した実装を完了したことを表す。
	StatusDone = "done"

	// StatusPassed は verify の検証に合格したことを表す。
	StatusPassed = "passed"

	// StatusFailed は verify の検証に不合格であり、実装の修正が必要であることを表す。
	StatusFailed = "failed"

	// StatusBlocked は人間の判断が必要で作業を中断したことを表す。
	StatusBlocked = "blocked"
)

// validStatuses は worker ごとに許可される status の集合である。
// work は「実装したか、実装できず止まったか」の二択、verify は
// 「合格・不合格・判断保留」の三択であり、worker が返してよい状態はそれぞれ異なる。
var validStatuses = map[string]map[string]bool{
	WorkerWork:   {StatusDone: true, StatusBlocked: true},
	WorkerVerify: {StatusPassed: true, StatusFailed: true, StatusBlocked: true},
}

// ValidStatus は worker が status を返してよいかどうかを判定する。
func ValidStatus(worker, status string) bool {
	m, ok := validStatuses[worker]
	return ok && m[status]
}

// StatusLine は GitHub コメントの 1 行目に埋め込まれる、nuage-autopilot 自身が
// 生成する状態行をパースした結果である。
type StatusLine struct {
	Worker string
	Status string

	// SHA は PR に対する実行の場合のみ埋まる。Issue に対する実行では空文字列のままとする。
	// stale 判定（状態行以降に新しいコミットが積まれたかどうか）に使う
	// （internal/cycle/transition.go 参照）。
	SHA string
}

var lineRe = regexp.MustCompile(`^<!--\s*nuage-autopilot\s+(.*?)\s*-->$`)

// Parse は body の 1 行目から nuage-autopilot の状態行を抽出する。
// 1 行目が状態行の形式でない場合、または worker/status が欠けている場合は ok=false を返す。
//
// この関数はコメントの投稿者を確認しない。人間が偶然似た文面を書いた場合と
// nuage-autopilot 自身の投稿を区別するのは呼び出し側の責務である
// （投稿者が botLogin と一致するかどうかを別途確認すること）。
func Parse(body string) (StatusLine, bool) {
	first := body
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		first = body[:i]
	}
	m := lineRe.FindStringSubmatch(strings.TrimSpace(first))
	if m == nil {
		return StatusLine{}, false
	}

	fields := make(map[string]string)
	for _, tok := range strings.Fields(m[1]) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		fields[k] = v
	}

	sl := StatusLine{Worker: fields["worker"], Status: fields["status"], SHA: fields["sha"]}
	if sl.Worker == "" || sl.Status == "" {
		return StatusLine{}, false
	}
	return sl, true
}

// Render は状態行 + summary からなるコメント本文全体を組み立てる。
// sha が空文字列の場合、sha= フィールド自体を省略する（Issue に対する実行）。
func Render(worker, status, sha, summary string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- nuage-autopilot worker=%s status=%s", worker, status)
	if sha != "" {
		fmt.Fprintf(&b, " sha=%s", sha)
	}
	b.WriteString(" -->\n")
	b.WriteString(summary)
	return b.String()
}

// Result は worker が NUAGE_REPORT_FILE に書き出す JSON の内容である。
// worker 自身の識別子は含まない（Go 側が起動時に worker を選んでいるため既知である）。
type Result struct {
	Status  string `json:"status"`
	Summary string `json:"summary"`
}

// ReadResultFile は path から Result を読み取る。
// status が空文字列であることは許容しない（worker が何も報告しなかった場合と
// 区別できないため）が、status がその worker にとって妥当な値かどうかまでは
// 検証しない（呼び出し側で ValidStatus を使って確認すること）。
func ReadResultFile(path string) (Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, fmt.Errorf("report: read result file: %w", err)
	}
	var res Result
	if err := json.Unmarshal(data, &res); err != nil {
		return Result{}, fmt.Errorf("report: decode result file: %w", err)
	}
	if res.Status == "" {
		return Result{}, errors.New(`report: result file is missing "status"`)
	}
	return res, nil
}
