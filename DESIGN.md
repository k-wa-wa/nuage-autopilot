# nuage-autopilot 設計書

GitHub Issue / PR を起点に、自律型 LLM CLI を駆動してアプリ開発を自動化する仕組み。
`nuage-agent` リポジトリの構想を、**完全に宣言的な NixOS サービス**として作り直したもの。

---

## 1. 目的と到達点

- Issue に「やりたいこと」を書けば、数十分後に PR と preview 環境が立ち上がっている状態を作る
- リスクの高い作業（インフラ・Talos・Terraform・SOPS）は従来通りローカル PC で人間と対話しながら行う
- アプリ層の変更（バグ修正・小さな機能追加）は自動化・省力化する

## 2. 決定事項

| 項目 | 決定 | 却下した選択肢と理由 |
| :-- | :-- | :-- |
| 実行基盤 | `dev-server` (NixOS VM) 上の systemd サービス | ローカル PC の常駐プロセス → 宣言的でない |
| 宣言性 | Nix で完全宣言。手で入れるのは secret のみ | — |
| 言語 | Go | TS/bun → Nix でのパッケージングが枯れていない |
| 実装場所 | `nuage-workspace/autopilot/` | 別リポジトリ → 横断管理の場に集約したい |
| 状態管理 | GitHub の Issue/PR ラベルのみ（DB なし） | — |
| プロセスモデル | `Type=oneshot` + `systemd.timers` | 常駐デーモン + 自作 crawler → 状態を持ち複雑 |
| 検証環境 | 既存の k8s preview (ArgoCD ApplicationSet) | EVPN Zone 払い出し → 過剰 |
| secret 注入 | 手動配置 + `EnvironmentFile` | sops-nix → 今回は不要 |
| 観測 | journald → promtail → Loki（既存スタック） | 自作 TUI → 捨てる |
| Holmes | 廃止する（別タスク） | — |

## 3. リポジトリ境界とデプロイ経路

`nuage-cluster/nix/flake.nix` は既に `nix-config.url = "github:k-wa-wa/nix-config"` を
外部 flake input として取り込んでいる。まったく同じ形で `nuage-workspace` を input にする。

```
nuage-workspace (public)
  flake.nix
    packages.x86_64-linux.nuage-autopilot   # buildGoModule
    nixosModules.nuage-autopilot            # services.nuage-autopilot
        │
        │ inputs.nuage-workspace
        ▼
nuage-cluster/nix/flake.nix
  nixosConfigurations.dev-server.modules += [ nuage-workspace.nixosModules.nuage-autopilot ]
        │
        │ master へ push → system.autoUpgrade (daily / OnBootSec 30s)
        ▼
dev-server 上で systemd サービスとして稼働
```

**注意**: 反映には `nuage-cluster` 側で `nix flake update nuage-workspace` による
`flake.lock` 更新が必要。この自動化は別途検討する。

## 4. ディレクトリ構成

```
nuage-workspace/
├── flake.nix                       # packages / nixosModules を export
├── autopilot/
│   ├── DESIGN.md                   # 本ファイル
│   ├── go.mod
│   ├── vendor/                     # コミットする (後述)
│   ├── cmd/nuage-autopilot/
│   │   └── main.go                 # エントリポイント
│   └── internal/
│       ├── config/                 # フラグ・環境変数の解決
│       ├── github/                 # Issue / PR / label 操作
│       ├── prompt/                 # 各エージェントのプロンプト定義
│       ├── runner/                 # LLM CLI (claude) の起動
│       └── cycle/                  # 1 サイクルの制御フロー
└── nix/
    └── modules/
        └── nuage-autopilot.nix     # NixOS モジュール
```

## 5. Go 実装の方針

### vendor ディレクトリをコミットする

`buildGoModule` は通常 `vendorHash` を要求し、`go.mod` を変更するたびにハッシュがズレて
ビルドが落ちる。**このシステムはエージェント自身が依存を追加する**ため、これは致命的である。

`go mod vendor` して `vendor/` をコミットし、`vendorHash = null` を指定することで
ハッシュ管理を完全に不要にする。依存差分が PR に乗る冗長さより、
「AI が依存を足してもビルドが壊れない」ことを優先する。

### 依存は最小限に保つ

標準ライブラリを基本とし、GitHub API は `github.com/google/go-github` を使う。
`gh` CLI に依存してもよいが、その場合は Nix モジュール側で `path` に含める。

## 6. Nix モジュール仕様

`services.nuage-autopilot` のオプション:

| オプション | 型 | 既定値 | 説明 |
| :-- | :-- | :-- | :-- |
| `enable` | bool | `false` | 有効化 |
| `package` | package | 本 flake の `nuage-autopilot` | 実行するパッケージ |
| `repositories` | listOf str | `[]` | 対象リポジトリ (`"k-wa-wa/pechka"` 形式) |
| `stateDir` | str | `/var/lib/nuage-autopilot` | 作業ディレクトリ |
| `interval` | str | `"*:0/5"` | systemd `OnCalendar` |
| `environmentFile` | str | `"-/var/lib/nuage-autopilot/secrets.env"` | secret の注入元 |
| `timeout` | str | `"30m"` | 1 サイクルの `TimeoutStartSec` |
| `extraPackages` | listOf package | `[]` | サービスの `path` に追加するツール |

### unit の生成方針

systemd の template unit (`nuage-autopilot@.service`) は使わず、
**`repositories` から 1 リポジトリにつき 1 組の service + timer を生成する**。

```
nuage-autopilot-pechka.service / .timer
nuage-autopilot-nuage-cluster.service / .timer
```

理由: instance 名に `/` を含められずエスケープが必要になること、
per-instance の `Environment=` 指定が煩雑になることを避けるため。
リポジトリ一覧はどのみち宣言的なので、生成してしまうほうが単純である。

### service の要件

- `Type = "oneshot"`
- `StateDirectory = "nuage-autopilot"`（`stateDir` に対応）
- `EnvironmentFile` は先頭 `-` 付き（ファイルが無くても起動失敗しない）
  - `nix/modules/common.nix` の `nix-daemon` の `EnvironmentFile` と同じイディオム
- `TimeoutStartSec` でハングを検知（旧 Supervisor の代替）
- `path` に `git` / `gh` / 必要なツールチェーンを含める
- `DynamicUser` は使わない（`git clone` と後の LLM CLI が HOME を要求するため）

### timer の要件

- `OnCalendar = cfg.interval`
- `Persistent = true`
- `RandomizedDelaySec` を入れてリポジトリ間で実行時刻をずらす

## 7. 実行モデル

1 回の起動 = 「対象リポジトリの Issue/PR を 1 周見て、処理すべきものがあれば処理して終了」。

プロセスは状態を持たない。状態は GitHub のラベルのみが保持する。
これにより旧実装の常駐デーモン・crawler・graceful shutdown・並列プール・Supervisor が
すべて systemd に吸収されて不要になる。

対象リポジトリの `git clone` は **実行時**に `stateDir` 配下で行う。
Nix の純粋性はビルドサンドボックス内の話であり、ランタイムの I/O には hash は不要。
毎回最新を取得するのが正しい。

## 8. ラベル状態機械（引き継ぐ設計）

旧 `nuage-agent` から引き継ぐ中核設計。

| ラベル | フェーズ | 動作 |
| :-- | :-- | :-- |
| `agent:spec` | 仕様定義 | 要求を PRD と受け入れ基準に落とす。曖昧なら質問して `agent:wait` |
| `agent:dev` | 開発 | ブランチを切り実装し、テスト通過まで自己修復して PR を作成 |
| `agent:review-general` | 一般レビュー | バグ・セキュリティ・性能を検証 |
| `agent:review-semantic` | 設計レビュー | 設計規約・影響範囲を検証 |
| `agent:qa` | 検証 | preview 環境に対する E2E を含む最終検証 |
| `agent:wait` | ユーザー待ち | 人間のコメントで解除 |
| `agent:triage` | 例外 | タイムアウト・ループ検知時のフォールバック |

## 9. テスト戦略

| 層 | 実行場所 | 内容 |
| :-- | :-- | :-- |
| L1 単体 | CI (GitHub Actions) | ビルド・単体テスト・lint |
| L2 結合 | CI | DB 等を含む結合テスト |
| L3 E2E | **preview namespace**（本番クラスタ） | `pechka-pr-N.cluster.wpc` への E2E |
| L4 探索的 | ローカル PC + AI エージェント | インフラ・破壊的作業 |

ローカル kind (`pechka/scripts/dev-up.sh`, `k8s/overlays/local`) は廃止候補とし、
preview に一本化して二重メンテを解消する。

## 10. 実装フェーズ

### Phase 1: 経路を通す（本タスクの範囲）

中身の実装より先に「push すれば dev-server に届く」経路を確立する。

- `autopilot/` の Go スケルトン（ビルドが通り、1 サイクル相当のログを出して正常終了する）
- `flake.nix` で `packages` と `nixosModules` を export
- `nix/modules/nuage-autopilot.nix` の実装

この段階では LLM CLI を呼ばない。`claude-code` パッケージの調達（後述）を Phase 1 の
リスクから外すため。

### Phase 2: GitHub 連携

Issue/PR の取得、ラベル判定、遷移処理の実装。

### Phase 3: LLM CLI 駆動

`claude -p`（headless）の起動とプロンプト定義の移植。

**要調達**: `nixpkgs 24.11` は EOL であり `claude-code` パッケージを含まない可能性が高い。
`nuage-cluster/nix/flake.nix` が `nixpkgs-ollama` から ollama だけを引いている前例と
同じ形で、`nixpkgs-unstable` から `claude-code` だけを引くのが最小コスト。

### Phase 4: preview 環境との接続

QA フェーズで `pechka-pr-N.cluster.wpc` に対して E2E を実行する。
dev-server から `*.cluster.wpc` を解決できる必要がある
（`chaos-monitor` の `networking.hosts` によるハードコードが前例）。

### Phase 5: 観測駆動

Alertmanager の webhook receiver を生やし、アラートから Issue を自動起票する。
Holmes を廃止したうえで、この経路に置き換える。

## 11. dev-server 側の必要変更（別タスク）

`nuage-cluster` リポジトリ側で行う。本タスクの範囲外だが、設計上の前提として記録する。

- `nix/flake.nix` の `inputs` に `nuage-workspace` を追加
- `nixosConfigurations.dev-server.modules` にモジュールと設定を追加
- secret ファイルを手動配置（`/var/lib/nuage-autopilot/secrets.env`）
- `*.cluster.wpc` の名前解決（Phase 4 で必要）
- VM スペックが LLM CLI の実行に耐えるかの確認（Terraform 層）

## 12. 制約

- コメント・ドキュメントは常体（である・する調）で記述する
- Git 操作（commit / push / branch）は指示がない限り行わない
- SOPS / Terraform / Terragrunt の操作は行わない
