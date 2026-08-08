# nuage-autopilot

GitHub の Issue / PR を起点に、自律型 LLM CLI（claude）を駆動してアプリ開発を自動化する
オートパイロットである。

Issue に要望を書くとエージェントが必要に応じて質問を返し、回答すると実装・レビューを済ませた
PR を作成する。人間が行うのは Issue を書くことと PR をマージすることだけになる、という運用を
目指している。

## ドキュメントの分担

| ファイル | 何が書いてあるか |
| :-- | :-- |
| README.md（本ファイル） | 何をするものか、動かし方、設定 |
| [DESIGN.md](./DESIGN.md) | アーキテクチャと、そう設計した理由。仕様として扱う |
| [AGENTS.md](./AGENTS.md) | このリポジトリで作業するエージェント向けの規約 |

## 動作の概要

単一の常駐プロセスが 4 つの goroutine を回す。GitHub の `/notifications` を 1 分ごとに
条件付きポーリングしてイベントを SQLite に積み、イベントがあるアイテムについてのみ
claude を起動する。**イベントが無ければ LLM も GitHub API もほぼ呼ばない。**

エージェントは `gh` で自由にコメント・Issue 起票・PR 作成を行い、結果の要約だけを
`NUAGE_REPORT_FILE` に JSON で残す。Go 側はそれを見て状態を進めるだけで、何をするかは
判断しない。詳細は [DESIGN.md](./DESIGN.md) を参照。

CI が緑になった PR は、続けて **verify** が検証する。verify はコードを変更しない別セッションで、
プレビュー環境を実際に叩いて要求どおり動いているかを確かめ、問題があれば PR にコメントして
エージェントに差し戻す。何をもって合格とするかは、対象リポジトリの `.agents/autopilot-verify.md` に書く
（無い場合、verify は判定を出さず PR をそのまま通す）。詳細は [DESIGN.md 8.4 節](./DESIGN.md)。

## 必要なもの

- `claude` CLI が実行ユーザーで認証済みであること（`~/.claude` などの CLI 側の設定を使う）
- `gh` CLI と `git`
- リポジトリへの権限を持つ `GH_TOKEN`

## ビルド・テスト

```sh
go build ./...
go test ./...
```

Nix でパッケージをビルドする場合は次を実行する。

```sh
nix build .#nuage-autopilot
```

## 実行

```sh
export GH_TOKEN=ghp_...
export GIT_AUTHOR_NAME=nuage-autopilot
export GIT_AUTHOR_EMAIL=autopilot@users.noreply.github.com

nuage-autopilot --repos owner/repo-a,owner/repo-b
```

`--repos` は必須である。ここに挙げたすべてのリポジトリが `NUAGE_STATE_DIR` 配下に clone され、
エージェントの作業ディレクトリの兄弟として並ぶ。

実装と verify で使う LLM CLI・モデルは役割ごとに指定できる。未指定なら claude を使い、
verify は実装エージェントと同じ設定になる。

```sh
nuage-autopilot --repos owner/repo \
  --verify-cli=agy --verify-model=gemini-3.1-pro-high
```

設定できる項目は [env.example](./env.example) に一覧がある。各項目の意味と既定値は
[DESIGN.md 13章](./DESIGN.md) にまとめてある。

### 初回起動時の挙動

起動した時点で既に存在していた open Issue は、記録されるだけで**着火しない**。
既存の Issue を処理させたい場合は、その Issue にコメントを 1 件書けばよい。

## 停止

`SIGTERM` / `SIGINT` で停止する。実行中の claude は停止させ、保持している lease を解放してから
終了する。中断された作業は次のイベントで再開する。
