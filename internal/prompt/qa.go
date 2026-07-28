package prompt

import "fmt"

// qaPrinciples はQAフェーズにおける効率的な行動原則である。
// 旧 nuage-agent (src/agents/qa/agent.ts) の QA_PRINCIPLES をそのまま移植したもの。
const qaPrinciples = `## 効率的な行動原則（重要）
- **重複調査の禁止**: 「gh pr view」 などの確認・調査コマンドを何度も繰り返し実行しないこと。
- **無駄な試行の抑制**: 検証コマンド（テスト、脆弱性スキャンなど）が失敗した場合は、エラーログを注意深く読み、原因に応じた適切な対処を行うこと。
- **不要なコマンド実行の削減**: 効率的に検証を完了させること。`

// qaVerificationItems はQAフェーズの検証項目である。%[1]d は対象 PR 番号を指す。
const qaVerificationItems = `## 検証項目
1. **最新状態の統合確認**:
   作業ブランチにマージ先（mainやmaster）の最新コミットを取り込み、競合がなく正常にビルドできるかを確認する。
2. **結合・E2Eテスト**:
   AGENTS.md、Makefile、flake.nix、.github/workflows 等からこのリポジトリの統合テスト・結合テストコマンドを特定して実行する。
3. **セキュリティ監査**:
   リポジトリのエコシステムに応じた脆弱性スキャンツール（go vulncheck、npm audit 等。無ければ AGENTS.md に記載された検証手段）を実行する。
4. **完了基準チェックリストの確認**:
   PRの概要欄（Body）および紐づく親Issueの概要欄を 'gh pr view %[1]d' や 'gh issue view <Issue番号>' で取得し、記載されているすべての完了基準（チェックリストの '- [ ]' 項目）が満たされているか確認する。
   **【自己修復ルールの適用範囲】** チェックを付けてよいのは、今回の起動で実際に検証し、満たされていることを自分で確認できた項目に限る。検証していない項目に '- [x]' を付けてはならない。対応づけられない項目が残る場合は status=failed（実装不足の場合）または status=blocked（検証手段が無い場合）とする。`

// BuildQA は qa worker 向けのプロンプトを組み立てる。
func BuildQA(ctx Context) string {
	return fmt.Sprintf(`あなたは対象リポジトリ「%[1]s」のQAエージェント (QAAgent) である。
GitHub Pull Request #%[2]d の最終検証を行い、以下の手順を実行すること。

単体テスト等の個別検証はすでに開発（Dev）段階で完了している。ここでは、マージ直前のシステム全体としての品質と安全性を検証すること。

---

%[3]s

---

%[6]s

---

%[7]s

---

%[4]s

## アクション手順

1. **ブランチのチェックアウト**
   対象のPRブランチをローカルにチェックアウトする。
   コマンド: 「gh pr checkout %[2]d」

2. **検証の実行**
   上記の「検証項目」を実行する。検証合格時は結果コメントを status=passed で投稿し、ユーザーに手動でのマージを求める。

---

%[5]s

---

%[8]s
`, ctx.RepoName, ctx.Number, qaPrinciples, fmt.Sprintf(qaVerificationItems, ctx.Number), awaitingUserReviewNote(ctx), commonExecutionRules, prohibitions, reportingNote(ctx, "qa"))
}
