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
| 実行基盤 | `autopilot-server` (NixOS VM) 上の systemd サービス | ローカル PC の常駐プロセス → 宣言的でない |
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
  nixosConfigurations.autopilot-server.modules += [ nuage-workspace.nixosModules.nuage-autopilot ]
        │
        │ master へ push → system.autoUpgrade (daily / OnBootSec 30s)
        ▼
autopilot-server 上で systemd サービスとして稼働
```

**注意**: 反映には `nuage-cluster` 側で `nix flake update nuage-workspace` による
`flake.lock` 更新が必要。この自動化は別途検討する。

## 4. ディレクトリ構成

```
nuage-workspace/
├── flake.nix                       # packages / nixosModules を export
├── autopilot/
│   ├── DESIGN.md                   # 本ファイル
│   ├── go.mod                      # 依存ゼロ (stdlib のみ) のため vendor/ は無い
│   ├── secrets.env.example
│   ├── cmd/nuage-autopilot/
│   │   └── main.go                 # エントリポイント
│   └── internal/
│       ├── config/                 # フラグ・環境変数の解決
│       ├── github/                 # Issue / PR / label 操作 (net/http)
│       ├── prompt/                 # 各フェーズのプロンプト定義
│       ├── repo/                   # 対象リポジトリの clone / 更新
│       ├── runner/                 # LLM CLI (claude) の起動
│       └── cycle/                  # 1 サイクルの制御フロー
└── nix/
    └── modules/
        └── nuage-autopilot.nix     # NixOS モジュール
```

## 5. Go 実装の方針

### vendorHash を持たない

`buildGoModule` は通常 `vendorHash` を要求し、`go.mod` を変更するたびにハッシュがズレて
ビルドが落ちる。**このシステムはエージェント自身が依存を追加する**ため、これは致命的である。

そこで `vendorHash = null` を指定してハッシュ管理を不要にする。
依存を追加する場合は `go mod vendor` した `vendor/` をコミットすることでこれが成立する。

### 依存は最小限に保つ

**実績として、外部依存はゼロで実装できている**（`go.mod` は stdlib のみ）。
GitHub API は `net/http` で直接叩き、git 操作と認証は `git` / `gh` CLI を
サービスの PATH 経由で呼ぶ。この状態を維持する限り `vendor/` は不要である。

依存を追加する場合は `go mod vendor` して `vendor/` をコミットすること。

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
| `user` | str | `"nixos"` | サービスの実行ユーザー。claude の認証情報を置く HOME の持ち主と一致させる |
| `extraPackages` | listOf package | `[]` | PATH に追加する Nix パッケージ |
| `extraPathPrefixes` | listOf str | `[]` | PATH に追加する Nix 管理外のディレクトリ。NixOS の `path` 仕様により末尾へ `/bin` が付与されるため、`/home/nixos/.local` のように親を指定する |

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

## 8. ディスパッチャ方式

### ラベルをプログラムカウンタにしない

旧 `nuage-agent` は「ラベルが次に実行すべきフェーズを保持し、実装はそれを読んで分岐する」
という設計だった。これはラベルを**プログラムカウンタ**として使うことに等しい。

ラベルは真の状態の写像にすぎず、必ずズレる（手動編集、遷移の取りこぼし、複数ラベルの同時付与）。
そこで本設計では **毎サイクル、現実から状態を導出し直す**。
PR が存在するか、CI が通っているか、未対応のレビュー指摘があるか — これらが真の状態である。

結果としてラベルは 2 つだけになり、いずれも「制御」の役割しか持たない。

| ラベル | 役割 | 外す人 |
| :-- | :-- | :-- |
| `agent:running` | ロック。実行中を示す | 人間（自動回収は将来課題） |
| `agent:awaiting_user_review` | ゲート。人間の対応待ち | 人間 |

**対象の選別はオプトアウト方式**とする。`agent:` ラベルが 1 つも付いていない open な
Issue/PR がすべて対象になる。エージェントに触らせたくないものには
`agent:awaiting_user_review` を人間が付けて止める。

`agent:awaiting_user_review` は worker 自身がプロンプト内で `gh` を叩いて付与する。
Go 側はこの付与に関与しない。解除は人間がラベルを外すことで行う。
コメントの投稿による自動解除は行わない（書きかけの返信で動き出す事故を防ぐため）。

### 1 サイクルの流れ

```
1. open な Issue/PR を取得し、agent: ラベルが付いているものを除外する
2. 残りが 0 件なら LLM を呼ばずに終了する
3. ループ上限に達しているアイテムがあれば agent:awaiting_user_review を付けて終了する
4. dispatcher (claude haiku) を 1 回だけ呼び、「どのアイテムを、どの worker に渡すか」を決めさせる
5. 選ばれたアイテムに agent:running を付ける
6. 対象リポジトリを clone / 更新し、worker (claude) を起動する
7. 終了したら agent:running を外す
```

dispatcher へは Issue/PR の**本文と直近のコメント履歴**を渡す。
番号・種別・タイトルだけでは「仕様が固まっているか」「レビューが通ったのか落ちたのか」を
判別できず、ルーティングを誤るためである。

ただし **clone はしない**。リポジトリの中身を読む必要があるのは worker であり、
dispatcher は GitHub 上の情報だけで判断できる。clone は worker が必要になった
時点（手順 6）で初めて行う。

本文とコメントは文字数で切り詰めたうえで渡す。切り詰めた場合はその旨が分かる形にし、
dispatcher が「情報が欠けている」と認識できるようにする。

### dispatcher の契約

- モデルは `claude-haiku-4-5-20251001` を明示指定する（判断のみで実装を伴わないため）
- **1 サイクルにつき 1 コール**。アイテムごとに呼ばない
- 出力は厳密な JSON とし、Go 側で検証する

```json
{ "number": 42, "kind": "issue", "worker": "dev", "reason": "..." }
```

- `worker` は `spec` / `dev` / `review` / `qa` / `none` のいずれか
- `number` と `kind` は手順 1 で取得した集合に含まれていなければならない
- パース失敗または不正値の場合は 1 回だけ再試行し、それでも駄目なら何もせず終了する
  （アイテムにラベルを付けないため、次サイクルで再度試行される）

### worker

Phase 3 で旧 `nuage-agent` から移植したプロンプトを worker として使う。
ただし **review-general と review-semantic は 1 つの `review` に統合**し、4 種類とする。
dispatcher が柔軟に選べる以上、一般レビューと設計レビューを別フェーズに分ける必然性が薄いため。

| worker | 役割 |
| :-- | :-- |
| `spec` | 要求を PRD と受け入れ基準に落とす。曖昧なら質問して `agent:awaiting_user_review` を付ける |
| `dev` | ブランチを切り実装し、テスト通過まで自己修復して PR を作成する |
| `review` | バグ・セキュリティ・性能に加え、設計規約・影響範囲を検証する |
| `qa` | preview 環境に対する E2E を含む最終検証を行う |

### ループ上限（Go 側の硬い制限）

「ユーザー待ち以外は処理を続ける」方針のため、終端に到達しないアイテムが
永久に回り続けうる。dispatcher の判断には依存せず、Go 側で確実に止める。

**判定**: アイテムのコメントを新しい順に見て、**最後の人間のコメント以降に投稿された
Bot コメントの数**を数える。これが上限（既定 5）以上なら
`agent:awaiting_user_review` を付けて、そのサイクルは worker を起動しない。

人間がコメントするとカウンタがリセットされる。「人間の関与が唯一の脱出口」という
モデルと一致する。Bot の判定には Phase 2 で実装した `CurrentUser()` と
コメント投稿者の `type` を用いる。

制約: worker が必ずコメントを残すとは限らないため、この計数は下限の近似である。
取りこぼしても実害は「もう少し回る」だけなので、当面はこれで十分とする。

### 将来課題: `agent:running` の自動回収

クラッシュや `TimeoutStartSec` による強制終了で `agent:running` が残ると、
そのアイテムは人間が外すまで対象外のままになる。当面は手動運用とする。

自動化する場合は **dispatcher とは別の systemd unit（janitor）** に分けるのが良い。
ラベルの後始末は判断を伴わないため LLM が不要で、決定的な Go の処理として書ける。

その際の注意点として、**janitor の閾値は `TimeoutStartSec` より確実に大きく取る必要がある**。
dev フェーズの実行中は Issue の `updated_at` が更新されないため、
閾値が短いと生きている実行からラベルを剥がし、同一アイテムに worker が二重に走る。

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

中身の実装より先に「push すれば autopilot-server に届く」経路を確立する。

- `autopilot/` の Go スケルトン（ビルドが通り、1 サイクル相当のログを出して正常終了する）
- `flake.nix` で `packages` と `nixosModules` を export
- `nix/modules/nuage-autopilot.nix` の実装

この段階では LLM CLI を呼ばない。`claude-code` パッケージの調達（後述）を Phase 1 の
リスクから外すため。

### Phase 2: GitHub 連携

Issue/PR の取得、ラベル判定、遷移処理の実装。

### Phase 3: LLM CLI 駆動

`claude -p`（headless）の起動とプロンプト定義の移植。**使用する CLI は claude のみ**とする
（旧 nuage-agent は QA を Antigravity で実行していたが、当面は claude に一本化する）。

claude は公式インストーラ（`curl | bash`）で導入し、TUI でサインインする。
配布物は generic Linux 向けの動的リンクバイナリであり、NixOS では
`programs.nix-ld` を有効にしないと実行できない
（`nuage-cluster/nix/hosts/autopilot-server/configuration.nix` で有効化済み）。

実体は `~/.local/bin/claude`（`~/.local/share/claude/versions/<version>` への symlink）で、
Nix パッケージではないため `services.nuage-autopilot.extraPathPrefixes` で
サービスの PATH に通す。

### Phase 4: preview 環境との接続

QA フェーズで `pechka-pr-N.cluster.wpc` に対して E2E を実行する。
autopilot-server から `*.cluster.wpc` を解決できる必要がある
（`chaos-monitor` の `networking.hosts` によるハードコードが前例）。

### Phase 5: 観測駆動

Alertmanager の webhook receiver を生やし、アラートから Issue を自動起票する。
Holmes を廃止したうえで、この経路に置き換える。

## 10.5 シークレットの取り扱い

GitHub / Claude / Antigravity のトークンは **SOPS で配布しない**。
流出時の影響が大きいため、VM 起動後に手作業で配置する運用とする。

- 配置先: `/var/lib/nuage-autopilot/secrets.env`（`services.nuage-autopilot.user` の所有 / `0600`）
- 参照方法: systemd の `EnvironmentFile`。先頭に `-` を付けるため、
  ファイルが存在しなくてもサービスは起動に失敗しない
- テンプレート: [`secrets.env.example`](./secrets.env.example)
- 書式は systemd の EnvironmentFile であり、シェルスクリプトではない
  （`export` を書かない、変数展開が効かない）

| 変数 | 用途 | 必要フェーズ |
| :-- | :-- | :-- |
| `GH_TOKEN` | gh CLI / GitHub API / git push の認証 | Phase 2 |
| `GIT_AUTHOR_NAME` / `GIT_AUTHOR_EMAIL` | 生成コミットの名義。committer にも同じ値を使う | Phase 3 |

`secrets.env` は誤コミットを防ぐためリポジトリの `.gitignore` に登録する。

### LLM CLI の認証は環境変数で渡さない

`claude` / `agy` は CLI の TUI でサインインし、認証情報を**実行ユーザーの HOME**
（`~/.claude` 等）に保存する。したがって API キーを `secrets.env` に置く必要はない。

代わりに、**人間がサインインするユーザーとサービスの実行ユーザーを一致させる**必要がある。
`services.nuage-autopilot.user`（既定 `nixos`）が `User=` に設定され、systemd が
passwd から `HOME` を設定する。root で動かすと、SSH でログインしてサインインした
ユーザーの認証情報を読めない。

サインインは VM 上で 1 回だけ行う。

```bash
ssh nixos@192.168.5.241
claude   # TUI でサインイン
```

### 必須環境変数が未設定のときの挙動

`secrets.env` は手作業で配置する運用のため、VM 作成直後は存在しない。
この状態を異常終了として扱うとタイマー実行のたびに service が failed になり、
本当の障害が埋もれる。そのため **警告ログを出して正常終了する**（終了コード 0）。

`EnvironmentFile` に `-` を付けてファイル不在を許容している設計と整合させた判断である。
必須変数は `internal/config` の `RequiredEnvVars` に定義する。

## 11. autopilot-server 側の構成（`nuage-cluster` リポジトリ）

VM は `terraform/vpc/zone-dev/autopilot-server.tf`、OS 構成は
`nix/hosts/autopilot-server/configuration.nix` で定義する。以下は構築済みである。

- `nix/flake.nix` の `inputs` に `nuage-workspace` を追加し、`nixosConfigurations.autopilot-server` を登録
- base-vm の qcow2 から起動し、cloud-init の hostname をもとに `nixos-bootstrap` が構成を自動適用
- `programs.nix-ld` を有効化（インストーラ版 claude の実行に必須）
- nameserver に lb の CoreDNS VIP `192.168.5.200` を指定。`cluster.wpc` を
  ワイルドカードで解決するため、PR ごとに変わる preview のホスト名にも到達できる
- `extraPathPrefixes = [ "/home/nixos/.local" ]` で claude を PATH に通す

人手が必要な作業:

- `secrets.env` の配置（`GH_TOKEN` / `GIT_AUTHOR_NAME` / `GIT_AUTHOR_EMAIL`）
- claude の TUI サインイン（`services.nuage-autopilot.user` と同一ユーザーで実行する）

### 反映手順

`nuage-workspace` と `nuage-cluster` の 2 リポジトリにまたがるため、順序を守る必要がある。

1. `nuage-workspace` を push
2. `nuage-cluster/nix` で `nix flake update nuage-workspace`
3. `nuage-cluster` を push
4. `sudo nixos-rebuild switch --refresh --flake "https://github.com/k-wa-wa/nuage-cluster/archive/master.tar.gz?dir=nix#autopilot-server"`

`--refresh` は必須である。Nix は tarball を `tarball-ttl`（既定 1 時間）の間
キャッシュするため、付けないと push 直後でも古い master を掴む。

## 12. 制約

- コメント・ドキュメントは常体（である・する調）で記述する
- Git 操作（commit / push / branch）は指示がない限り行わない
- SOPS / Terraform / Terragrunt の操作は行わない
