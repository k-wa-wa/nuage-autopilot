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
        │
        │ inputs.nuage-workspace
        ▼
nuage-cluster/nix/
  hosts/autopilot-server/configuration.nix で nuage-workspace.packages.* を参照し
  systemd.services.nuage-autopilot を定義
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
├── flake.nix                       # packages を export
└── autopilot/
    ├── DESIGN.md                   # 本ファイル
    ├── go.mod                      # 依存ゼロ (stdlib のみ) のため vendor/ は無い
    ├── secrets.env.example
    ├── cmd/nuage-autopilot/
    │   └── main.go                 # エントリポイント
    └── internal/
        ├── config/                 # フラグ・環境変数の解決
        ├── github/                 # Issue / PR / label 操作 (net/http)
        ├── prompt/                 # 各 worker (work/verify) のプロンプト定義
        ├── report/                 # worker <-> nuage-autopilot 間の結果受け渡しプロトコル
        ├── repo/                   # 対象リポジトリの clone / 更新
        ├── runner/                 # LLM CLI (claude) の起動
        └── cycle/                  # 1 サイクルの制御フロー（遷移表・dispatcher）
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

## 6. systemd サービス仕様

NixOS モジュール化は行わず、`nuage-cluster` 側の `hosts/autopilot-server/configuration.nix` にて `systemd.services.nuage-autopilot` を直接定義する。

### 構成方針

単一の `nuage-autopilot.service` unit を定義し、`--repos` 引数にカンマ区切りで対象リポジトリ一覧（`"k-wa-wa/pechka,k-wa-wa/nuage-cluster,..."`）を渡すことで、1 回の起動で全リポジトリを直列に巡回・処理する。

```nix
systemd.services.nuage-autopilot = {
  description = "nuage-autopilot: 全リポジトリを巡回して 1 サイクルを実行する";
  after = [ "network-online.target" ];
  wants = [ "network-online.target" ];
  path = [ pkgs.git pkgs.gh "/home/nixos/.local" ];
  environment.NUAGE_STATE_DIR = "/var/lib/nuage-autopilot";
  serviceConfig = {
    Type = "oneshot";
    StateDirectory = "nuage-autopilot";
    EnvironmentFile = "-/var/lib/nuage-autopilot/secrets.env";
    TimeoutStartSec = "30m";
    ExecStart = "${lib.getExe pkg} --repos ${reposArg}";
    User = "nixos";
  };
};
```

### service の要件

- `Type = "oneshot"`
- `StateDirectory = "nuage-autopilot"`（`/var/lib/nuage-autopilot` ディレクトリを作成する）
- `EnvironmentFile` は先頭 `-` 付き（ファイルが存在しなくても起動失敗させない）
  - `nix/modules/common.nix` の `nix-daemon` の `EnvironmentFile` と同じイディオムを使用する
- `TimeoutStartSec` でハングを検知する（旧 Supervisor の代替）
- `path` に `git` / `gh` / `"/home/nixos/.local"`（claude インストーラの配置先）を含める
- `DynamicUser` は使わない（`git clone` と LLM CLI が固定の HOME を要求するため）
- 現在 timer unit は未設定であり、`systemctl start nuage-autopilot` による手動実行（または必要に応じて定期実行用タイマーを追加設定）で運用する


## 7. 実行モデル

1 回の起動 = 「対象リポジトリの Issue/PR を 1 周見て、処理すべきものがあれば処理して終了」。

プロセスは状態を持たない。状態は GitHub のラベルのみが保持する。
これにより旧実装の常駐デーモン・crawler・graceful shutdown・並列プール・Supervisor が
すべて systemd に吸収されて不要になる。

対象リポジトリの `git clone` は **実行時**に `stateDir` 配下で行う。
Nix の純粋性はビルドサンドボックス内の話であり、ランタイムの I/O には hash は不要。
毎回最新を取得するのが正しい。

## 8. 遷移表と worker 選択

### ラベルをプログラムカウンタにしない

旧 `nuage-agent` は「ラベルが次に実行すべきフェーズを保持し、実装はそれを読んで分岐する」
という設計だった。これはラベルを**プログラムカウンタ**として使うことに等しい。

ラベルは真の状態の写像にすぎず、必ずズレる（手動編集、遷移の取りこぼし、複数ラベルの同時付与）。
そこで本設計では **毎サイクル、現実から状態を導出し直す**。
CI が通っているか、状態行が何を報告しているか、状態行以降に新しいコミットが積まれたか、
未対応の人間コメントがあるか — これらが真の状態である。

結果としてラベルは 2 つだけになり、いずれも「制御」の役割しか持たない。

| ラベル | 役割 | 外す人 |
| :-- | :-- | :-- |
| `agent:running` | ロック。実行中を示す | 人間（自動回収は行わない。「他プロセスが起動している可能性」を Go 側からは判別できないため） |
| `agent:awaiting_user_review` | ゲート。人間の対応待ち | 人間 |

**対象の選別はオプトアウト方式**とする。`agent:` ラベルが 1 つも付いていない open な
Issue/PR がすべて対象になる。エージェントに触らせたくないものには
`agent:awaiting_user_review` を人間が付けて止める。

`agent:awaiting_user_review` は worker 自身ではなく **nuage-autopilot (Go) が** 付与する
（`status="blocked"` の報告を受けたとき、または worker が有効な報告を残せなかったとき。
「状態行プロトコル」節を参照）。解除は人間がラベルを外すことで行う。
コメントの投稿による自動解除は行わない（書きかけの返信で動き出す事故を防ぐため）。

### なぜ遷移表なのか

「次にどの worker を起動すべきか」の大半は、CI 状態・状態行・コミット SHA・関連 PR の
有無から機械的に導出できる。これを毎回 LLM (dispatcher) に自然言語のルールとして
判断させるのは、有限状態機械を確率的な手段で再実装しているに等しく、パース失敗や
解釈揺れによる空転を生む。

そこで本設計では、この導出を Go の**遷移表** (`internal/cycle/transition.go`) として
決定的に実装する。dispatcher (LLM) が呼ばれるのは、遷移表が「直近の人間コメントの
意図を読む必要がある」と判定した場合（`ask`）に限られる。日常的なサイクル（CI 待ち、
verify 合格待ちのマージ待ち、work 完了後の verify 起動など）は **LLM を一切呼ばない**。

### 1 サイクルの流れ

```
1. open な Issue/PR を取得し、agent: ラベルが付いているものを除外する
2. 残りが 0 件なら LLM を呼ばずに終了する
3. ループ上限に達しているアイテムがあれば agent:awaiting_user_review を付けて終了する
4. 残った候補それぞれについて遷移表を評価する
   -> work / verify が機械的に決まった候補（decisive）と、
      人間コメントの意図を読む必要がある候補（ask）に分かれる
5. decisive な候補があれば、その中から 1 件選ぶ（PR を優先し、同種なら updated_at が
   古いものを優先する）
6. decisive な候補が無く ask な候補がある場合のみ、それらを dispatcher (claude haiku)
   に渡して 1 件選ばせる
7. 選ばれたアイテムに agent:running を付ける
8. 対象リポジトリを clone / 更新し、worker (claude) を起動する
9. worker が残した報告を読み、結果コメントの投稿と（必要なら）
   agent:awaiting_user_review の付与を nuage-autopilot 自身が行う
10. agent:running を外す
```

### worker

4 フェーズ（spec/dev/review/qa）は **`work` と `verify` の 2 つに統合**した。
フェーズを跨ぐたびに引き継げるコンテキストが GitHub コメント（文字数で切り詰め済み）
経由に限られ、目減りしていたことが最大の理由である。

| worker | 対象 | 役割 |
| :-- | :-- | :-- |
| `work` | Issue / PR | 要求を理解し、実装し、テストが通る状態にして PR を作成・更新する。要求が曖昧な場合は実装せず `status="blocked"` とする（旧 `spec` の役割を内包） |
| `verify` | PR のみ | コードは一切変更せず、差分の静的レビュー（バグ・セキュリティ・性能・設計規約・影響範囲）と実行検証（統合テスト・E2E・完了基準チェック）を行う（旧 `review` + `qa` の統合） |

`work` が Issue から実装した PR を作る場合、PR 本文に必ず `Closes #<issue番号>` を
含めさせる。Issue ↔ PR の紐付け（`related_open_prs` / 遷移表の「関連 PR」判定）は
この記法を正規表現で抽出することで決定的に行う（`internal/cycle/dispatcher.go` の
`extractRelatedIssueNumbers`）。

### 状態行プロトコル（`internal/report`）

worker は結果コメントを自身で投稿しない。**投稿するのは常に nuage-autopilot (Go)
自身である。** worker がプロンプト内の `gh` コマンドでコメントを投稿していた旧方式は、
書式の崩れや無言終了のたびに状態行が失われ、ループ上限判定や次サイクルの判断を
狂わせていた。

worker は、環境変数 `NUAGE_REPORT_FILE` が指すパスに、終了前に次の JSON を書き出すだけでよい。

```json
{ "status": "done", "summary": "実装した内容・検証結果・次のステップの要約" }
```

nuage-autopilot はこのファイルを読み、状態行 + summary からなるコメントを組み立てて
GitHub に投稿する。

```html
<!-- nuage-autopilot worker=<work|verify> status=<done|passed|failed|blocked> sha=<40hex> -->
```

- `sha` は PR に対する実行のみ含める（Issue では省略）。worker 実行後、GitHub から
  権威ある head SHA を再取得して埋める（`internal/github.Client.GetPullRequest`）。
  これが遷移表の「状態行以降に新しいコミットが積まれたか」判定の基礎になる
- `status` に許される値は worker ごとに異なる（`internal/report.ValidStatus`）
  - `work`: `done`（完了） / `blocked`（人間の判断が必要）
  - `verify`: `passed`（合格） / `failed`（不合格、実装の修正が必要） / `blocked`
- `blocked` の場合、nuage-autopilot が `agent:awaiting_user_review` を付与する

report ファイルが存在しない・JSON として不正・その worker にとって妥当でない
`status` である場合、nuage-autopilot は **`status="blocked"` を自ら合成**して投稿し、
`agent:awaiting_user_review` を付与する。worker の無言終了・書式崩れが致命的に
ならないようにするための設計であり、これによりループ上限判定（Bot コメント数）が
確実に機能する。

### 遷移表（`internal/cycle/transition.go`）

各候補について、コメント履歴から**最新の自分自身（botLogin）の状態行**と、
**それより新しい人間コメントの有無**を導出したうえで（`deriveState`）、以下の表を
上から順に評価し、最初に一致した結果を採用する。

#### PR

| # | 条件 | 結果 |
| :-- | :-- | :-- |
| 1 | draft である | `none` |
| 2 | 状態行より新しい人間コメントがある | `ask` |
| 3 | CI が pending | `none` |
| 4 | CI が failure | `work` |
| 5 | 状態行が無い | `verify`（新規 PR、CI は通っている） |
| 6 | 状態行の sha が現在の head SHA と異なる | `verify`（状態行以降に新しいコミットがある） |
| 7 | 状態行が `blocked` | 状態行の worker に差し戻す（ラベル解除後の再開） |
| 8 | 状態行が `verify status=failed` | `work` |
| 9 | 状態行が `work status=done` | `verify` |
| 10 | 状態行が `verify status=passed` | `none`（終端。人間のマージ待ち） |
| 11 | それ以外 | `none`（安全側。ループ上限が backstop） |

人間コメント（#2）を CI 状態（#3, #4）より優先する。CI が落ちていても人間が
「まだ触らないで」と言っていれば、機械的な `ci=failure -> work` で上書きしてはならない。

`verify status=passed` の PR に人間が追加 push した場合、ラベル等で候補から
除外することはしない。#6 の sha 比較が自動的に検知し、`verify` に戻す。

#### Issue

| # | 条件 | 結果 |
| :-- | :-- | :-- |
| 1 | 関連する open PR がある | `none`（PR 側で進む） |
| 2 | 状態行より新しい人間コメントがある | `ask` |
| 3 | 状態行が無い | `work`（未着手） |
| 4 | 状態行が `blocked` | 状態行の worker に差し戻す |
| 5 | 状態行が `work status=done` | `none`（PR を伴わない完了。人間の対応待ち） |
| 6 | それ以外 | `none` |

### dispatcher の契約

dispatcher が呼ばれるのは、遷移表が `ask` と判定した候補が存在し、かつ機械的に
決まる（decisive な）候補が 1 件も無いときだけである。判断の対象は 1 点のみ：
**直近の人間コメントの意図が「修正の指示」「再検証の依頼」「対応不要」のどれか**。
CI 状態や関連 PR の有無による自動ルーティングは遷移表の責務であり、dispatcher の
プロンプトには含めない。

- モデルは `claude-haiku-4-5-20251001` を明示指定する（判断のみで実装を伴わないため）
- **1 サイクルにつき 1 コール**。候補ごとに呼ばない
- 出力は厳密な JSON とし、Go 側で検証する

```json
{ "number": 42, "kind": "issue", "worker": "work", "reason": "..." }
```

- `worker` は `work` / `verify` / `none` のいずれか
- `number` と `kind` は渡した候補集合（`ask` と判定されたものだけ）に含まれていなければならない
- パース失敗または不正値の場合は 1 回だけ再試行し、それでも駄目なら何もせず終了する
  （アイテムにラベルを付けないため、次サイクルで再度試行される）

### ループ上限（Go 側の硬い制限）

「ユーザー待ち以外は処理を続ける」方針のため、終端に到達しないアイテムが
永久に回り続けうる。dispatcher・遷移表の判断には依存せず、Go 側で確実に止める。

**判定**: アイテムのコメントを新しい順に見て、**最後の人間のコメント以降に投稿された
Bot コメントの数**を数える。これが上限（既定 5）以上なら
`agent:awaiting_user_review` を付けて、そのサイクルは worker を起動しない。

人間がコメントするとカウンタがリセットされる。「人間の関与が唯一の脱出口」という
モデルと一致する。Bot の判定には Phase 2 で実装した `CurrentUser()` と
コメント投稿者の `type` を用いる。

制約: worker が必ず report ファイルを残すとは限らないが、その場合も nuage-autopilot が
合成した `blocked` コメントを必ず投稿するため（「状態行プロトコル」節）、この計数の
取りこぼしは実質的に生じない。

### `agent:running` の自動回収は行わない

クラッシュや `TimeoutStartSec` による強制終了で `agent:running` が残ると、
そのアイテムは人間が外すまで対象外のままになる。

自動回収（例: 起動時に無条件で全リポジトリの `agent:running` を剥がす）は、
**他プロセスが同時に稼働している可能性がある運用**では安全ではないため採用しない。
`Type=oneshot` の単一 systemd unit 前提であれば「起動時点で見つかった
`agent:running` は残骸である」と言えるが、この前提が崩れる運用形態
（手動での並行実行、複数ホストからの起動等）を将来にわたって排除できない限り、
自動回収はラベルの二重付与・worker の二重起動という事故につながる。
当面は手動運用とする。

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
- `flake.nix` で `packages` を export
- `nuage-cluster` 側の `hosts/autopilot-server/configuration.nix` で systemd サービスを定義

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
Nix パッケージではないため systemd サービスの `path` に `"/home/nixos/.local"` を含めて
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

- 配置先: `/var/lib/nuage-autopilot/secrets.env`（ systemd サービスの `User` (`nixos`) の所有 / `0600`）
- 参照方法: systemd の `EnvironmentFile`。先頭に `-` を付けるため、
  ファイルが存在しなくてもサービスは起動に失敗しない
- テンプレート: [`secrets.env.example`](./secrets.env.example)
- 書式は systemd の EnvironmentFile であり、シェルスクリプトではない
  （`export` を書かない、変数展開が効かない）

| 変数 | 用途 | 必要フェーズ |
| :-- | :-- | :-- |
| `GH_TOKEN` | gh CLI / GitHub API / git push の認証 | Phase 2 |
| `GIT_AUTHOR_NAME` / `GIT_AUTHOR_EMAIL` | 生成コミットの名義。committer にも同じ値を使う | Phase 3 |
| `NUAGE_ALLOWED_AUTHORS` | 対象とする Issue/PR の作成者カンマ区切りリスト（既定: `k-wa-wa,bot-wa-wa`） | Phase 3 |

`secrets.env` は誤コミットを防ぐためリポジトリの `.gitignore` に登録する。

### LLM CLI の認証は環境変数で渡さない

`claude` / `agy` は CLI の TUI でサインインし、認証情報を**実行ユーザーの HOME**
（`~/.claude` 等）に保存する。したがって API キーを `secrets.env` に置く必要はない。

代わりに、**人間がサインインするユーザーとサービスの実行ユーザーを一致させる**必要がある。
サービスの `User`（`nixos`）が `User=` に設定され、systemd が
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
- systemd サービスの `path` に `"/home/nixos/.local"` を含めて claude を PATH に通す

人手が必要な作業:

- `secrets.env` の配置（`GH_TOKEN` / `GIT_AUTHOR_NAME` / `GIT_AUTHOR_EMAIL`）
- claude の TUI サインイン（systemd サービスの `User = "nixos"` と同一ユーザーで実行する）

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
