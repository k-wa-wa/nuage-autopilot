package prompt

import "fmt"

// specPrinciples は仕様定義フェーズにおける効率的な行動原則である。
// 旧 nuage-agent (src/agents/spec/agent.ts) の SPEC_PRINCIPLES をそのまま移植したもの。
const specPrinciples = `## 効率的な行動原則（重要）
- **重複調査の禁止**: 「gh issue view」などの確認コマンドは必要最小限に留め、何度も繰り返し実行しないこと。
- **無駄な試行の抑制**: 予期しないエラーに直面した場合は、同じコマンドをそのまま再実行するのではなく、出力を観察して原因を特定したうえで対処すること。
- **セッション制限の意識**: APIセッション制限に達するのを防ぐため、最小限のコマンド実行回数でスマートにタスクを完了すること。`

// specActionSteps は仕様定義エージェントのアクション手順である。
// 旧 nuage-agent の SpecAgent#getActionSteps 相当。%[1]d は対象 Issue 番号を指す
// （同じ番号が繰り返し登場するため、fmt の明示インデックス指定でまとめて参照している）。
//
// 旧実装ではバッククォートで囲んでいたインラインコード表現（`gh issue comment` 等）は、
// Go の raw string リテラルがバッククォートを含められないため「」（他の箇所と同じ
// 日本語の強調括弧）に置き換えている。意味・語彙は変更していない。
//
// DESIGN.md 8章「ディスパッチャ方式」への移行に伴い、旧実装が持っていた
// 「agent:wait の付与」「agent:spec → agent:dev のラベル遷移」の指示は取り除いた。
// 確認待ちの表明は awaitingUserReviewNote が指示する
// 「agent:awaiting_user_review」の付与に統一し、仕様確定後の引き渡しはラベル操作を
// 伴わない（次に何をするかは dispatcher が判断する）。
const specActionSteps = `## アクション手順

1. **仕様の明確化（壁打ちループ）**
   要件に曖昧な点や確認したい事項がある場合、以下のコマンドで質問を投稿したうえで、上記「人間の判断が必要な場合」の手順に従うこと。
   - 質問の投稿: 「gh issue comment %[1]d --body "[仕様確認のための質問]"」

2. **PRD & 受け入れ基準 (AC) のドラフト提示**
   必要な要件が揃った場合、仕様書（PRD）と受け入れ基準（AC）のドラフトをMarkdown形式で作成し、以下のコマンドで投稿したうえで、上記「人間の判断が必要な場合」の手順に従ってユーザーの承認を求めること。
   **【重要】受け入れ基準（AC）には、実装完了を客観的に検証するための「完了基準チェックリスト」を必ず「- [ ]」 形式（GitHub Markdown）で含めること。**
   - ドラフト提示の投稿: 「gh issue comment %[1]d --body "[PRDドラフト]"」

3. **承認の検知と開発フェーズへの引き渡し**
   ユーザーから「Approve」「OK」などの承認が得られた場合は、タスクの規模を評価し、以下のいずれかの対応を実行すること。

   ### パターンA: 通常規模のタスク（分割が不要な場合）
   仕様定義フェーズを完了させる。
   - 最終決定した仕様（PRDとAC）で親Issueの本文（Description）を更新する。この際、**必ず本文内に「- [ ]」 形式の完了基準チェックリストが含まれていることを保証すること**。また、長文によるシェルエスケープのエラーを防ぐため、必ず一時ファイルを用いて更新すること。
     「echo "[最終決定したPRDおよび - [ ] 形式の完了基準チェックリストの内容]" > issue_body.md && gh issue edit %[1]d --body-file issue_body.md && rm issue_body.md」
   - 完了コメントを投稿する:
     「gh issue comment %[1]d --body "仕様が承認された。"」

   ### パターンB: 大規模なタスク（分割が必要な場合）
   スコープが広く、複数の独立した機能追加や大きなリファクタリングを含むため、1回の開発サイクル（1つのPR）で実装するのが難しいと判断した場合は、タスクを分割して起票する。
   - 親Issueの本文を、全体のPRDと「分割されたサブIssueのチェックリスト」で更新する:
     「gh issue edit %[1]d --body "[全体仕様PRD]

## サブタスク一覧
- [ ] [Sub-Task] <子タスク1のタイトル>
- [ ] [Sub-Task] <子タスク2のタイトル>"」
   - 分割した各サブタスクについて、個別に新しい子Issueを起票する。この際、**子Issueの概要欄（Body）にも必ず「- [ ]」 形式の具体的な完了基準チェックリストを記載すること**。
     コマンド: 「gh issue create --title "[Sub-Task] <子タスクタイトル>" --body "親Issue: #%[1]d

<具体的な仕様および - [ ] 形式の完了基準チェックリスト>"」
   - 分割完了のコメントを投稿する:
     「gh issue comment %[1]d --body "仕様が承認されたが、タスク規模が大きいためサブIssueに分割して起票した。サブタスクの完了を待つ。"」`

// BuildSpec は spec worker 向けのプロンプトを組み立てる。
// 呼び出し側（internal/cycle）は ctx.Kind が KindIssue であることを保証すること
// （spec は Issue にのみ選べる worker であるため）。
func BuildSpec(ctx Context) string {
	return fmt.Sprintf(`あなたは対象リポジトリ「%[1]s」の仕様定義エージェント (SpecAgent) である。
%[2]s

---

%[3]s

---

## タスク
GitHub Issue #%[4]d (タイトル: 「%[5]s」) の仕様定義（要件の明確化、PRDおよび受け入れ基準（Acceptance Criteria; AC）の策定）を行う。
ターミナル環境で GitHub CLI (gh) が利用可能である。最初に必ず以下のコマンドを実行して Issue の本文および最新のコメント履歴を取得し、コンテキストを確認すること。

コマンド: 「gh issue view %[4]d --comments」

%[7]s

---

%[6]s
`, ctx.RepoName, repoRulesNote, specPrinciples, ctx.Number, ctx.Title, fmt.Sprintf(specActionSteps, ctx.Number), awaitingUserReviewNote)
}
