// Package prompt は各 worker（spec/dev/review/qa）の LLM CLI (claude) 向け指示
// プロンプトを組み立てる。
//
// 旧 nuage-agent リポジトリ（src/agents/*/agent.ts）の buildPrompt 相当を Go へ移植した
// ものであり、日本語のプロンプト本文は原文をできる限り維持している。
//
// 旧実装との差分は次の3点である。
//   - リポジトリ構成マップ（repoMapMd）の注入をやめ、対象リポジトリの clone 内にある
//     AGENTS.md を claude 自身に読ませる前提に置き換えた（repoRulesNote を参照）。
//   - レビュー系フェーズ（review-general/qa）は旧実装では Antigravity (agy) で実行して
//     いたが、DESIGN.md フェーズ3の決定に従い claude に統一したため、Runner の選択に
//     関する記述はプロンプト本文から取り除いている。
//   - DESIGN.md 8章「ディスパッチャ方式」への移行に伴い、各 worker のプロンプトから
//     フェーズ遷移のためのラベル付け替え指示（agent:spec → agent:dev 等）を取り除いた。
//     次に何をするかは dispatcher が毎サイクル判断するため、worker はラベル操作に
//     関与する必要が無くなった。ただし人間の対応が必要な場合の
//     agent:awaiting_user_review の付与のみは worker 自身の役割として残している
//     （awaitingUserReviewNote 参照）。
package prompt

import "fmt"

// Kind は Build 対象が Issue か PullRequest かを表す。
// internal/cycle の itemKind と同じ値集合を持つが、prompt パッケージが cycle パッケージに
// 依存する（import する）ことを避けるため独立した型として定義している。
type Kind string

const (
	KindIssue       Kind = "issue"
	KindPullRequest Kind = "pull_request"
)

// Context は各 worker のプロンプトを組み立てるために必要な最小限の情報である。
// 旧 nuage-agent の AgentContext から repoMapMd（リポジトリ構成マップ）と
// autoMerge（QA成功時の自動マージ可否）を除いたものに相当する。
// 前者は AGENTS.md を claude 自身に読ませる方針への変更により、後者は
// DESIGN.md に自動マージの記述がないため常に人間の手動マージを前提とする方針への
// 変更により、それぞれ不要になった（詳細は各 Build 関数のコメントを参照）。
type Context struct {
	// RepoName は "owner/name" 形式の対象リポジトリ名である。
	RepoName string

	// Kind は対象が Issue か PullRequest かを表す。
	Kind Kind

	// Number は対象の Issue/PR 番号である。
	Number int

	// Title は対象の Issue/PR タイトルである。
	Title string
}

// repoRulesNote は、旧 nuage-agent が生成していたリポジトリ構成マップ（repo-map）の
// 注入に代わる一文である。DESIGN.md および実装指示に従い、リポジトリ固有のルールの
// 読み込みは Go 側で内容を組み立てて注入するのではなく、clone 済みのリポジトリから
// claude 自身に読ませる。
const repoRulesNote = `対象リポジトリのルート直下に AGENTS.md が存在する場合、作業に着手する前に必ずその内容を読み込み、そこに記載された固有のルール・規約を遵守すること。`

// awaitingUserReviewNote は、作業の続行に人間の判断が必要になった場合の対応を
// 指示する一文である。DESIGN.md 8章「agent:awaiting_user_review は worker 自身が
// プロンプト内で gh を叩いて付与する」に対応する。4 種類すべての worker プロンプトに
// 含める。
//
// gh issue edit / gh pr edit は番号を種別付きで解決するため、ctx.Kind に応じた
// 正しいコマンドを生成する。
func awaitingUserReviewNote(ctx Context) string {
	cmd := fmt.Sprintf("gh issue edit %d --add-label \"agent:awaiting_user_review\"", ctx.Number)
	if ctx.Kind == KindPullRequest {
		cmd = fmt.Sprintf("gh pr edit %d --add-label \"agent:awaiting_user_review\"", ctx.Number)
	}

	return fmt.Sprintf(`## 人間の判断が必要な場合
要件があいまいで確認が必要、あるいは自律的に判断してよい範囲を超える意思決定が必要になった場合は、作業を中断し、以下のコマンドで「agent:awaiting_user_review」ラベルを付与したうえで、その理由をコメントで説明すること。
コマンド: 「%s」
このラベルは人間が手動で外すまで解除されない。ラベルを外すまで再びこのアイテムが処理されることは無いため、安易に付けず、本当に人間の判断が必要な場合にのみ使用すること。`, cmd)
}
