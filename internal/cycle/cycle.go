// Package cycle は 1 サイクル（対象リポジトリの Issue/PR を 1 周見て、処理すべきものが
// あれば処理して終了する処理単位）の制御フローを持つ。
//
// Phase 1 の時点では GitHub 連携も LLM CLI の起動も行わない。ここでは関数境界のみを
// 用意し、Phase 2 以降で internal/github・internal/prompt・internal/runner を呼び出す
// 実装に差し替える受け皿とする。
package cycle

import (
	"context"
	"time"
)

// Result は 1 サイクルの実行結果を表す。Phase 1 では入力パラメータと開始時刻を
// そのまま返すのみで、実際の Issue/PR 処理は行わない。
type Result struct {
	// Repo は処理対象のリポジトリ（owner/name 形式）。
	Repo string

	// StateDir はサイクル実行に使用した作業ディレクトリ。
	StateDir string

	// StartedAt はサイクル開始時刻。
	StartedAt time.Time
}

// Run は repo に対する 1 サイクルを実行する。
//
// Phase 1 では実処理を行わず、呼び出しパラメータと開始時刻を記録した Result を
// 返すのみである。Phase 2 以降でここに GitHub の Issue/PR 取得・ラベル判定・
// 遷移処理・LLM CLI 起動を実装する。
func Run(ctx context.Context, repo, stateDir string) (Result, error) {
	result := Result{
		Repo:      repo,
		StateDir:  stateDir,
		StartedAt: time.Now(),
	}

	return result, nil
}
