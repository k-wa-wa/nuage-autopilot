package prompt

import "fmt"

// workEfficiencyPrinciples は work フェーズにおける効率的な行動原則である。
// 旧 nuage-agent (src/agents/dev/agent.ts) の EFFICIENCY_PRINCIPLES を移植したもの。
const workEfficiencyPrinciples = `## 効率的な行動原則（重要）
- **重複調査の禁止**: 「git status」 や 「git log」 などの確認コマンドを何度も繰り返し実行しないこと。
- **無駄な試行の抑制**: 同じエラーに対して単にコマンドを再実行するのではなく、エラーログから根本原因（型エラー、設定ミスなど）を特定して速やかにコードや設定ファイルを修正すること。同じアプローチで3回以上失敗した場合は、別のアプローチを検討すること。
- **不要なコマンド実行の削減**: 効率的に実装を進めること。`

// workCodeVerificationProcess は実装後のローカル検証手順である。
const workCodeVerificationProcess = `修正完了後、必ずこのリポジトリのテストと Lint を実行すること。
   - **コマンドを決め打ちにしないこと**: 対象リポジトリは Node.js とは限らない（Go / Nix / Shell / 設定のみのリポジトリもある）。AGENTS.md、README、Makefile、justfile、package.json、flake.nix、CI 定義（.github/workflows）からこのリポジトリで実際に使われているコマンドを特定して実行すること。
   - **テストを実行せずに完了を報告してはならない**: 検証手段を特定できない場合は、その事実を summary に明記したうえで status="blocked" とすること。
   - エラーが発生した場合は自律的に原因を特定して修正を繰り返す。ただし同じアプローチで 3 回以上失敗した場合は別のアプローチを検討し、それでも解消しなければ status="blocked" として終了すること。`

// workAmbiguityNote は要求が曖昧な場合の扱いを指示する一文である。
// 旧 nuage-agent の spec フェーズが担っていた「要件の明確化」を、独立したフェーズとして
// 持つのではなく、work の冒頭判断に内包したものである。
const workAmbiguityNote = `## 要求が曖昧な場合
実装に進む前に、要件・受け入れ基準が実装可能な程度に明確かどうかを判断すること。曖昧な点がある場合は、実装や作業ブランチの作成を行わず、status="blocked" として、具体的に何を確認したいのかを summary に書くこと。`

// BuildWork は work worker 向けのプロンプトを組み立てる。
// Issue（新規実装）と PullRequest（既存 PR への追加対応。verify 不合格・CI 失敗・
// 人間からの修正指示のいずれかが理由）の両方をこの 1 関数で扱う。
func BuildWork(ctx Context) string {
	var taskSection, processSection string

	switch ctx.Kind {
	case KindPullRequest:
		taskSection = fmt.Sprintf(`## タスク
GitHub Pull Request #%[1]d (タイトル: 「%[2]s」) に対応する。この PR は既に存在する。
上記の本文、および提示されていれば直近の検証結果・直近の人間コメントを踏まえ、何を修正・追加すべきかを判断すること。`, ctx.Number, ctx.Title)

		processSection = fmt.Sprintf(`## 開発・送信プロセス

1. **作業ブランチのチェックアウト**
   対象の PR ブランチをローカルにチェックアウトする。
   コマンド: 「gh pr checkout %[1]d」

2. **コード修正とローカル検証**
   %[2]s

3. **修正内容のプッシュ**
   ローカルテストが完全に通過したら、変更をコミットしてリモートにプッシュする（通常は PR ブランチにそのまま push する）。新しい PR を作成してはならない。`, ctx.Number, workCodeVerificationProcess)

	default: // KindIssue
		taskSection = fmt.Sprintf(`## タスク
GitHub Issue #%[1]d (タイトル: 「%[2]s」) に記載された要求を実装する。
作業を開始する前に、まず「gh pr list --search "%[1]d in:body"」を実行して既に本 Issue に紐づく open な PR が存在しないか確認すること。既に PR が作成されている場合は、二重作成を避け、その旨を summary に書いて status="blocked" で終了すること。`, ctx.Number, ctx.Title)

		processSection = fmt.Sprintf(`## 開発・送信プロセス

1. **作業ブランチの作成**
   実装を開始する前に、最新の main/master ブランチから「feature/issue-%[1]d」という新しい作業ブランチを作成する。
   コマンド: 「git checkout -B feature/issue-%[1]d」

2. **コード実装とローカル検証**
   %[2]s

3. **Pull Request の作成**
   ローカルテストが完全に通過したら、変更をリモートにプッシュし、Pull Request (PR) を作成する。
   - PR のタイトル: 「feat: #%[1]d %[3]s」
   - PR の概要欄（Body）には、必ず「Closes #%[1]d」を含めること（Issue との自動的な紐付けに必須であり、次サイクルの判断がこれに依存する）。`, ctx.Number, workCodeVerificationProcess, ctx.Title)
	}

	return fmt.Sprintf(`あなたは nuage-autopilot の work フェーズを担当するエージェントである。対象リポジトリ「%[1]s」に対して、要求を理解した上で実装し、テストが通る状態にして PR を作成または更新することが役割である。
%[2]s

---

%[3]s

---

%[4]s

---

%[5]s

---

%[6]s

---

%[7]s

---

%[8]s

---

%[9]s

---

%[10]s
`, ctx.RepoName, repoRulesNote, workAmbiguityNote, workEfficiencyPrinciples, contextSection(ctx), taskSection, processSection, commonExecutionRules, prohibitions, reportNote([]string{"done", "blocked"}))
}
