# nuage-autopilot

GitHub の Issue / PR を起点に、自律型 LLM CLI（claude 等）を駆動してアプリ開発を自動化するオートパイロットである。

Issue に要望を書くとエージェントが必要に応じて質問を返し、回答すると実装・レビューを済ませた PR を作成する。人間が行うのは Issue を書くことと PR をマージすることだけになる、という運用を目指す。設計の背景・詳細は [DESIGN.md](./DESIGN.md) を参照。

## 構成

- `cmd/nuage-autopilot`: 実行バイナリのエントリポイント。poll/work/resync/watchdog の 4 goroutine を常駐させる単一プロセス。
- `internal/`: 各サブシステムの実装（`github` / `ingest` / `store` / `engine` / `runner` / `daemon` / `config` / `prompt` など）。
- `flake.nix`: Nix パッケージ定義（`nuage-autopilot` パッケージをビルド）。
- `secrets.env.example`: 実行に必要なシークレットのテンプレート。実際の値はコミットせず、配置手順に従って対象ホストへ手作業で置く。

## ビルド・テスト

```sh
cd autopilot
go build ./...
go test ./...
```

Nix でパッケージをビルドする場合は次を実行する。

```sh
nix build ./autopilot#nuage-autopilot
```
