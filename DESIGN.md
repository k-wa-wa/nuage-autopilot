# nuage-autopilot 設計書

GitHub Issue / PR を起点に、自律型 LLM CLI を駆動してアプリ開発を自動化する仕組み。

---

## 1. 目的と到達点

Issue に「やりたいこと」を曖昧なまま書けば、エージェントが質問を返し、回答すれば実装と
レビューを済ませた PR が上がってくる。preview を見てフィードバックを書けば PR が修正される。
人間が明示的に行うのは「書くこと」と「マージすること」だけである。

リスクの高い作業（インフラ・Talos・Terraform・SOPS）は従来通りローカル PC で人間と対話
しながら行う。自動化の対象はアプリ層の変更に限る。

## 2. ユーザーストーリー（これが仕様である）

1. GitHub 上で曖昧に issue を起票する
2. エージェントが裏で動き、質問をしてくる
3. issue 上で質問に回答する
4. エージェントが裏で動き、実装しレビューまで済ませた PR を作成する
5. （スコープ外）PR に基づいて preview 環境が自動で立ち上がる
6. preview を見てフィードバックする
7. エージェントが裏で動き、PR を修正する
8. 人間がマージする

Issue が大きい場合は、エージェントがサブ Issue に分割しながら進める。

この 8 ステップのうち、**2・4・7 以外のすべてが人間の行動によって駆動される**。
本設計はこの構造をそのまま実装に写し取る。

## 3. 設計の中核にある 3 つの判断

### 3.1 イベント駆動にする

ストーリーの遷移は、すべて人間か CI のアクションに対応する。

| ストーリー | 対応するイベント |
| :-- | :-- |
| 1. issue 起票 | issue が open された |
| 3. 質問に回答 | 人間がコメントした |
| 6. preview を見て FB | 人間がコメント / レビューした |
| 8. マージ | PR が close / merge された |
| （実装後の合否） | CI チェックが完了した |

イベントを取り込めば「何が起きたか」が最初から手に入る。**イベントが無ければ LLM も
GitHub API もほぼ呼ばない**。これがコスト削減の最大のレバーである。

全コメントの取得やパースによる状態復元を不要とし、差分イベントのみに基づいてリアクティブに動作する。

### 3.2 状態は SQLite に持つ

状態を SQLite に保存することで以下が可能になる。


| 目的・メリット | 詳細 |
| :-- | :-- |
| TTL 付きリース | プロセスがクラッシュしても期限切れにより安全に自動回収される |
| Issue の親子関係 | 木構造を管理し、サブ Issue 分割と連携を可能にする |
| claude セッションの継続 | `--resume` により前回の文脈を保持して作業を継続できる |
| 予算（コスト・実行回数）の蓄積 | ドルベースの実コストと実行回数による正確な安全網を提供する |
| 「回答待ち」をラベルで表現しない | 人間のコメント投稿イベントによって自律的に処理が再開される |

**DB は真実ではない。GitHub が真実である。** したがって低頻度の全走査（resync）による
修復を必ず併走させる。これは Kubernetes のコントローラが watch と定期 resync を
併用しているのと同じ構造である。

### 3.3 エージェントを自由にし、Go は「起こすか否か」だけを決める

Go が決めるのは **「起こすか否か」だけ**とする。何をするかはエージェントが決める。

- worker は **1 種類**に統合する（将来 verify のみ独立させる。8.4 節）
- エージェントは `gh` で自由にコメント・Issue 起票・ラベル操作を行ってよい
- 1 起動で実装 → テスト → 自己レビュー → PR まで走り切ってよい
- 「今回は何もしない」を選んでよい

イベント駆動を採用することで、Go 側の遷移表は非常にシンプルになる。
「新しい人間コメントがあるか」「新しいコミットが積まれたか」は、イベントそのものが直接通知するため、複雑な差分解析は不要である。

## 4. 決定事項

| 項目 | 決定 | 却下した選択肢と理由 |
| :-- | :-- | :-- |
| 実行基盤 | `autopilot-server` (NixOS VM) 上の systemd サービス | ローカル PC の常駐プロセス → 宣言的でない |
| 宣言性 | Nix で完全宣言。手で入れるのは secret のみ | — |
| 言語 | Go | TS/bun → Nix でのパッケージングが枯れていない |
| 実装場所 | `nuage-workspace/autopilot/` | 別リポジトリ → 横断管理の場に集約したい |
| 状態管理 | SQLite（`/var/lib/nuage-autopilot/state.db`） | ラベル + コメント状態行 → 表現力とコストの両面で限界 |
| イベント取り込み | `GET /notifications` の条件付きポーリング | 後述（5 節） |
| プロセスモデル | 単一バイナリの常駐プロセス（`Type=notify`）+ goroutine | oneshot × 3 unit + timers → Nix 側の記述が増え、プロセス間で SQLite を奪い合う |
| LLM CLI | claude のみ | Antigravity → 一本化する |
| 検証環境 | 既存の k8s preview (ArgoCD ApplicationSet) | EVPN Zone 払い出し → 過剰 |
| secret 注入 | 手動配置 + `EnvironmentFile` | sops-nix → 今回は不要 |
| 観測 | journald → promtail → Loki（既存スタック） | 自作 TUI → 捨てる |

### イベント取り込み手段の選定

クラスタ内に外部からの inbound 経路が無いため、**notifications の条件付きポーリング**を採用する。
ポーリングは stateless かつ self-healing であり、人間ペースの開発サイクル（数分〜数十分）において 1 分程度の遅延は実用上の支障にならない。
なお、取り込み手段は拡張可能な構造（7.1 節）とし、必要に応じて webhook receiver 等を同じ `events` テーブルへの供給源として追加できるようにする。

## 5. アーキテクチャ

単一バイナリの常駐プロセス 1 つで構成し、内部を 4 つの goroutine に分ける。

```
                      nuage-autopilot （単一プロセス）
  ┌──────────────────────────────────────────────────────────┐
  │                                                          │
GitHub ──→ [poller]      1 分ごと。notifications を取り込む   │
  │           │                                              │
  │           ├─ events に enqueue ─→ SQLite ←──────┐        │
  │           └─ chan で通知 ─┐                     │        │
  │                           ▼                     │        │
  │         [worker]  通知 or 1 分ごと。1 件処理     │        │
  │           │                                     │        │
  │           ▼                                     │        │
  │         claude （1 アイテム = 1 セッション）      │        │
  │           ├─→ gh でコメント・Issue 起票・PR 作成 （人間チャネル）
  │           └─→ NUAGE_REPORT_FILE に outcome      （機械チャネル）
  │                                                 │        │
  │         [resyncer] 1 時間ごと。全走査して修復 ───┘        │
  │                                                          │
  │         [watchdog] 30 秒ごとに sd_notify WATCHDOG=1       │
  └──────────────────────────────────────────────────────────┘
```

**単一プロセスにする理由**は 3 つある。

- 状態を SQLite に持つことで、常駐しても**プロセス自体は無状態**を維持できる
- Nix 側の構成定義が 1 つの systemd サービスのみでシンプルになる
- SQLite を単一プロセス内の `*sql.DB` 1 つで共有できるため、複数プロセス間でのロック競合が発生しない

poller は enqueue 後にチャネルで worker を起こす。イベントが積まれた瞬間に処理が
始まるため、ポーリング間隔ぶんの待ちが発生しない。チャネルはバッファ 1 のノンブロッキング
送信とし、worker が取りこぼしても次の定期起床（1 分）が拾う。

worker が 30 分かかっている間も poller と resyncer は動き続ける。

### ハング検知

常駐プロセスにおけるハング検知のため、3 層の保護機構を設ける。

| 層 | 対象 | 手段 |
| :-- | :-- | :-- |
| claude の実行 | エージェント 1 回の実行 | `context.WithTimeout`（既定 120 分）。`exec.CommandContext` が子プロセスを殺す |
| HTTP | GitHub API 呼び出し | `http.Client.Timeout` |
| プロセス全体 | デッドロック・goroutine の停止 | systemd の `WatchdogSec` + `sd_notify` |

watchdog goroutine は、poller / worker / resyncer がそれぞれ更新する
「最終生存時刻」（atomic）を確認し、すべてが期待間隔内に動いている場合にのみ
`WATCHDOG=1` を送る。停止していれば送信をやめ、systemd がプロセスを再起動する。

`sd_notify` は `$NOTIFY_SOCKET` への datagram 送信であり、外部依存を必要としない
（数十行の自前実装で足りる）。

### 停止時の挙動

`SIGTERM` を受けたら新規のエージェント起動を止め、実行中のものには猶予を与えて待つ。
猶予を超えたら claude を kill し、**保持している lease を解放してから終了する**。

lease は TTL を持つため、解放できずに終了しても最終的には回収される。
明示的な解放は「再起動後すぐに再開できる」ようにするための最適化にすぎない。

## 6. 状態モデル

### 6.1 phase と lease は直交する

- **phase** はストーリー上どこまで進んだかを表す永続的な状態である
- **lease** は「今この瞬間、誰かが作業中か」を表す一時的な排他制御である

両者を混同しないため、`working` に相当する phase は**持たない**。これにより、
プロセスがクラッシュして lease が期限切れになっても phase はそのまま残り、
次の実行が自然に再開できる。手動でのラベル回収等の運用は一切不要である。

### 6.2 phase

| phase | 意味 | 何を待っているか |
| :-- | :-- | :-- |
| `new` | 認識したが未着手 | 着火 |
| `awaiting_answer` | エージェントが質問した | **人間の回答** |
| `in_review` | PR がある。CI・検証・修正を反復中 | CI 完了 / 検証 / 人間の FB |
| `ready` | 実装が済み CI も緑（将来はここに verify 合格が加わる） | **人間のマージ** |
| `blocked` | 人間の判断が必要 | **人間の対応** |
| `delegated` | サブ Issue に分割済み | 子の完了 |
| `done` | close / merge 済み | — |

**`awaiting_answer` がラベルではなく phase であることが、ストーリー 2 → 3 を成立させる。**
人間が issue に回答すれば、そのコメントがイベントになり、そのまま次に進む。
人間が剥がすべきラベルは存在しない。

### 6.3 スキーマ

```sql
PRAGMA user_version = 1;

CREATE TABLE items (
  id           INTEGER PRIMARY KEY,
  repo         TEXT    NOT NULL,           -- "owner/name"
  number       INTEGER NOT NULL,
  kind         TEXT    NOT NULL,           -- issue | pull_request
  phase        TEXT    NOT NULL,
  parent_id    INTEGER REFERENCES items(id),
  session_id   TEXT,                       -- claude --resume 用
  head_sha     TEXT,
  cost_usd     REAL    NOT NULL DEFAULT 0,
  runs         INTEGER NOT NULL DEFAULT 0,
  last_seen_at TEXT,                       -- 取り込み済みコメントの最新時刻
  updated_at   TEXT    NOT NULL,
  UNIQUE(repo, number)
);

CREATE TABLE events (
  id           INTEGER PRIMARY KEY,
  dedup_key    TEXT    NOT NULL UNIQUE,    -- "comment:<id>" 等
  item_id      INTEGER NOT NULL REFERENCES items(id),
  type         TEXT    NOT NULL,
  actor        TEXT    NOT NULL,
  body         TEXT,
  created_at   TEXT    NOT NULL,
  processed_at TEXT                        -- NULL = 未処理（これがキューである）
);

CREATE TABLE leases (
  item_id    INTEGER PRIMARY KEY REFERENCES items(id),
  holder     TEXT NOT NULL,                -- host:pid
  expires_at TEXT NOT NULL
);

CREATE TABLE cursors (
  source        TEXT PRIMARY KEY,          -- "notifications"
  etag          TEXT,
  last_modified TEXT,
  since         TEXT,
  polled_at     TEXT
);
```

`events.processed_at IS NULL` がそのまま処理待ちキューになる。別途キュー機構を持たない。

`dedup_key` の UNIQUE 制約が冪等性を保証する。同じコメントを何度取り込んでも
イベントは 1 件しか生まれない。webhook を追加した場合は delivery ID をここに入れる。

### 6.4 SQLite ドライバと vendorHash

**pure-Go の実装（`github.com/ncruces/go-sqlite3`）を使う。** cgo が入ると
`buildGoModule` でのビルドと後述の `vendorHash = null` 運用が破綻するためである。

`modernc.org/sqlite` は GOOS/GOARCH の組み合わせごとに C→Go
変換済みコードを vendor するため `vendor/` が 200MB を超える。一方 `ncruces/go-sqlite3` は
SQLite 本体を WebAssembly にコンパイルしたバイナリ 1 つを `go:embed` で組み込む方式
（wazero による pure-Go の WASM ランタイムで実行する）であり、vendor サイズが
約 14MB に収まる。ビルド・実行時ともネットワークアクセスは不要である。

`buildGoModule` は通常 `vendorHash` を要求し、`go.mod` を変更するたびにハッシュがズレて
ビルドが落ちる。**このシステムはエージェント自身が依存を追加する**ため、これは致命的である。
そこで `vendorHash = null` を指定してハッシュ管理を不要にし、`go mod vendor` した
`vendor/` をコミットすることでこれを成立させる。

`go.mod` の `go` ディレクティブは nixpkgs 24.11 が同梱する Go（1.23.8）を超えない
バージョンに固定する必要がある。依存を `go get <pkg>@latest` で追加すると、
Go ツールチェイン自身が要求する `go` ディレクティブを引き上げてしまうことがあるため、
追加のたびに `go.mod` の `go` 行を確認し、必要なら依存を 1 つ前のマイナーバージョンへ
固定する。

SQLite ドライバの追加に伴い `vendor/` のコミットを必須としている。依存はこれ以外に
増やさない方針を維持する。GitHub API は `net/http` で直接叩き、git 操作と認証は
`git` / `gh` CLI をサービスの PATH 経由で呼ぶ。

## 7. イベント取り込み

### 7.1 source は差し替え可能にする

```
[notifications poller] ─┐
[webhook receiver]     ─┼──→  events テーブル  ──→  以降は共通
[resync sweeper]       ─┘
```

イベントの正規化さえ揃えれば、取り込み手段を追加・変更・併走させても下流
（phase 遷移・lease・エージェント起動）は一切変わらない。

### 7.2 notifications ポーリングの手順

```
1. GET /notifications?since=<cursor>  （If-Modified-Since / If-None-Match 付き）
      → 304 なら即終了。rate limit を消費しない
2. 200 → 更新されたスレッド一覧を得る
3. 変化したアイテムについてのみ GET /issues/{n}/comments?since=<item.last_seen_at>
4. actor == bot login のものを捨てる
5. 残りを events に enqueue し、item.last_seen_at と cursor を更新する
```

3 が notifications 方式の追加コストだが、**変化したアイテムにしか発生しない**。
静かな時間帯は 1 の 304 だけで終わる。1 分間隔で回しても 1 日 1440 回の
実質無料リクエストに収まる。

スレッドを既読にする `PATCH` は行わない（書き込みリクエストを増やさないため）。
`since` パラメータとアイテムごとの `last_seen_at` を watermark として使い、
重複は `dedup_key` が吸収する。

### 7.3 自分のコメントで自分を起こしてはならない

**これは設計上の必須要件である。** エージェントが `gh` でコメントを投稿すると、
それ自体が notification を発生させる。フィルタしなければポーラーがそれを拾い、
エージェントを起こし、またコメントし、**無限ループする**。

webhook なら payload の `sender` を見れば済むが、`/notifications` はスレッド単位でしか
返さないため、必ず 7.2 の手順 3 まで降りて投稿者を確認する必要がある。

判定は bot login（`GET /user` の結果。プロセス内でキャッシュしてよい）との一致、
および投稿者の `type == "Bot"` で行う。

自身および Bot による投稿をフィルタリングしない場合、無駄なエージェント起動や無限ループが発生するため、この判定はシステム安定稼働のための必須要件である。

### 7.4 CI の完了は notifications では取れない

`check_suite` / `check_run` は notification に来ない。`phase = in_review` のアイテムに
限って `GET /repos/{repo}/commits/{sha}/check-runs` を直接取得する。

対象は通常 0〜2 件しかないため、コストは有界に収まる。状態が前回から変化した場合のみ
`ci_success` / `ci_failure` イベントを enqueue する。

### 7.5 resync

**取り込み手段が何であれ、整合性の最終保証は定期的な全走査である。**
webhook は配信に失敗しうるし、トンネルは切れるし、notifications は取りこぼす。

毎時 1 回、全対象リポジトリの open な Issue/PR を走査して次を行う。

- DB に無いアイテムを登録する（**着火はしない**。7.6 節を参照）
- GitHub 上で close / merge 済みのアイテムを `done` にする
- `head_sha` の乖離を修正する
- 期限切れの lease を削除する

### 7.6 初回起動時に一斉着火しない

DB が空の状態で起動すると、既存の open Issue すべてが「新規」に見える。そのまま
着火すると多数のエージェントが同時に走り、コストが跳ねる。

**初めて認識したアイテムは `new` として記録するだけで着火しない。**
着火するのは cursor 以降に発生したイベントを持つアイテムのみである。
つまり「autopilot を有効にした後に起票された Issue」から動き始める。

既存の Issue を対象にしたい場合は、人間がその Issue にコメントを 1 件書けばよい
（それがイベントになる）。

## 8. 遷移とエージェント

### 8.1 遷移表（LLM を呼ばない）

Go が決めるのは「起こすか否か」だけである。

| phase | イベント | 動作 |
| :-- | :-- | :-- |
| `new` | opened / commented | エージェント起動（新規セッション） |
| `awaiting_answer` | commented（人間） | エージェント起動（resume） |
| `blocked` | commented（人間） | エージェント起動（resume） |
| `in_review` | ci_failure | エージェント起動（resume） |
| `in_review` | ci_success | `ready` へ遷移（**将来ここに verify が入る**。8.4 節） |
| `in_review` | commented / reviewed（人間） | エージェント起動（resume） |
| `ready` | commented / reviewed（人間） | エージェント起動（resume） |
| `ready` | ci_failure（人間の追加 push） | エージェント起動（resume）。`in_review` へ戻す |
| `delegated` | child_done（全子完了） | エージェント起動（resume） |
| `done` | 任意 | 無視 |
| 任意 | closed / merged | 記録のみ。`done` へ |
| 任意 | イベント無し | **何もしない** |

イベント本文や投稿者情報を直接利用するため、人間のコメント意図を事前にLLMで事前分類するアプローチはとらない。
イベント情報をそのままエージェントに渡すことで、LLM 呼び出し回数を抑え、分類ミスによる誤動作を防ぐ。

### 8.2 チャネルを 2 本に分ける

「人間への伝達」と「機械可読な状態の受け渡し」の責務を分離する。

| | 宛先 | 中身 | 書き手 |
| :-- | :-- | :-- | :-- |
| **人間チャネル** | GitHub の comment / PR body / Issue | 自由。長さも書式も回数も制限しない | エージェント |
| **機械チャネル** | `NUAGE_REPORT_FILE` | 極小の構造化データ | エージェント |

エージェントの投稿内容や書式に依存せず、DB 側の phase は安全に維持される。
エージェントは `gh` を使用して柔軟にコメントや修正を投稿できる。

### 8.3 エージェントの契約

Go が渡すのは「このアイテムでこのイベントが起きた」だけである。返してもらうのは
phase を進めるための最小情報だけである。

```json
{ "outcome": "asked", "children": [12, 13] }
```

| outcome | 意味 | 遷移先 |
| :-- | :-- | :-- |
| `asked` | 人間に質問した（本文は既に GitHub へ投稿済み） | `awaiting_answer` |
| `implemented` | 実装し PR を作成・更新した | `in_review` |
| `split` | サブ Issue に分割した | `delegated` |
| `blocked` | 人間の判断が必要で中断した | `blocked` |
| `idle` | 対応不要と判断した | 変化なし |

`summary` フィールドは持たない。人間向けの文章は既に GitHub に投稿されているためである。

**Go が GitHub に書き込むのは失敗経路だけである。** エージェントが何も投稿せず report も
残さずに終了した場合に限り、Go が短い定型文を投稿して `blocked` にする。
これは無言終了に対する唯一の保険であり、通常運転では Go は何も書き込まない。

結果として、blocked の説明文は「実際に作業したエージェントが書いたもの」になり、原因や状態を的確に把握できる。

### 8.4 verify（将来の拡張枠）

現行ではエージェントの自己レビュー（実装・テスト・自己レビュー・PR作成を一括実行）で完結させるが、将来的な別セッションによる第三者レビュー（`verify`）の追加に備えて以下のインターフェース枠を保持する。

1. **実行モードの区別**: `internal/runner` および `internal/prompt` に `ModeAgent` / `ModeVerify` の区別を持たせる（現在は `agent` のみ実装）。
2. **`ready` phase の独立**: `in_review` + `ci_success` から `ready` へ遷移する構造を保ち、将来 verify を差し込める設計とする。

### 8.5 何を解放し、何を締めるか

締める基準は「判断力を信用しないから」ではなく**不可逆だから**とする。

| 解放する | 締める |
| :-- | :-- |
| `gh issue comment` / `gh pr comment` | 既定ブランチへの直 push・force push |
| `gh issue create`（サブ Issue） | ブランチ・タグの削除 |
| `gh issue edit` / `gh pr edit`（ラベル含む） | 他者のコメントの編集・削除 |
| PR の作成・更新・本文編集 | 他者の PR / Issue の close |
| 1 起動で実装 → テスト → 自己レビュー → PR まで走り切る | secret の閲覧・標準出力への出力・コミット |
| 実行中に何度でもコメントする | SOPS / Terraform / Terragrunt の実行 |
| 「今回は何もしない」を選ぶ | GitHub Actions の secrets・ワークフロー権限の変更 |
| セッション継続による長期記憶 | Issue/PR 本文の指示による上記の迂回 |

ラベルは DB が状態を持つためプログラムカウンタとしては使用しない。エージェントに自由に付けさせてよい（状態制御は lease と phase が担う）。

人間が「これには触るな」と示すための `agent:ignore` のみ、Go が読み取り専用で参照する
オプトアウト用マーカーとして残す。

### 8.6 セッションの継続

`items.session_id` を保存し、`claude -p --resume <id>` で再開する。エージェントは
前回の自分の質問・試行・判断を保持したまま作業を続けられるため、毎回ゼロから Issue を
読み直す探索ターンが消える。

claude のセッションは作業ディレクトリに紐づく。clone のパスは
`/var/lib/nuage-autopilot/<owner>/<name>` で安定しているため resume が成立する。

セッションが肥大化するとコンテキストとコストが増えるため、実行回数または経過時間で
打ち切り、新規セッションに切り替える。

## 9. サブ Issue への分割

エージェントが `outcome = "split"` を返し、作成した Issue 番号を `children` に入れる。
Issue の起票自体はエージェントが `gh issue create` で行う。

Go 側の処理は次の通りである。

- `children` を `items` に登録し、`parent_id` を親に向ける
- 親を `phase = delegated` にする

**親が `delegated` である限り、親に対して直接エージェントを起動しない。** これにより
「親と子で同じものを二重に実装する」事故が構造的に防がれる。

全子が `done` になった時点で `child_done` イベントを親に enqueue し、親のエージェントを
resume する。親は統合・クローズ・追加分割のいずれかを判断する。

`child_done` の dedup_key には「今回全完了の引き金になった子の item id」を含める
（`child_done:<parent_id>:<child_id>`）。親 id だけをキーにすると、親が将来再び分割し
子が再び全完了した場合に 2 回目以降の完了が dedup で握りつぶされてしまうためである。

`items` は `repo` を持つため、スキーマ上は親子がリポジトリを跨ぐことを表現できる。
ただし `outcome="split"` の `children` は Issue 番号の配列のみでリポジトリ情報を
持たない（8.3 節）ため、**初版ではエージェントが `gh issue create` するサブ Issue は
常に親と同じリポジトリであることを前提とする**。リポジトリを跨ぐ分割をサポートする
場合は `children` を `{"repo": "...", "number": N}` の配列に拡張する必要がある
（現時点では未対応）。GitHub ネイティブの sub-issues API は表示上の紐付けとして
併用してよいが、真実の所在は DB とする。

## 10. 予算と安全網

実測コストベースの安全網を導入する。

- `claude --output-format json` が返す `total_cost_usd` を `items.cost_usd` に累積する
- `items.runs` に実行回数を累積する
- 上限（既定 **$5 / 10 runs**）に達したら `phase = blocked` にし、エージェントを起動しない
- **人間がコメントすると両方をリセットする**

「人間の関与が唯一の脱出口である」という安全モデルを採用する。
実コストと実行回数を参照することで、安全網として正しく機能し、かつ autopilot のランニングコストが可視化される。

## 11. リースによる排他

エージェントを起動する前に `leases` に行を挿入する（`item_id` が PRIMARY KEY なので
二重取得は SQLite が防ぐ）。`expires_at` はエージェント 1 回の実行タイムアウト
（既定 120 分。12 節）より長く取る。

- 正常終了時に削除する
- クラッシュ時は期限切れによって自動的に回収される
- phase は lease と独立しているため、回収後は元の phase から自然に再開する

`holder` と `expires_at` を持たせることで、判定と回収を安全に行う。

## 12. 実行モデル

| goroutine | 起床契機 | 処理 | 想定所要時間 |
| :-- | :-- | :-- | :-- |
| poller | 1 分ごと | notifications を取り込み events に enqueue | 数秒（304 なら 1 秒未満） |
| worker | poller からの通知、または 1 分ごと | 未処理イベントを 1 件処理する | 数分〜数十分 |
| resyncer | 1 時間ごと | 全走査して DB を修復する | 数十秒 |
| watchdog | 30 秒ごと | 他 3 つの生存を確認し `sd_notify` する | 即時 |

いずれも `time.Ticker` と `select` による単純なループとし、`ctx.Done()` で停止する。
間隔は環境変数で上書きできるようにする（開発時に短縮するため）。

worker は 1 回の起床で**イベントを 1 件だけ**処理する。並行実行は行わない
（clone を共有するためワーキングツリーが衝突する）。並行化が必要になった場合は
`git worktree` によるアイテムごとの作業ツリー分離に移行する。

`items` / `events` の更新は SQLite の WAL モードで行い、`busy_timeout` を設定する。
単一プロセス内なので `*sql.DB` を 1 つ共有すれば Go のコネクションプールが直列化する。

対象リポジトリの `git clone` は実行時に `stateDir` 配下で行う。Go は clone を
既定ブランチの最新状態に戻すところまでを担い、PR のチェックアウトはエージェントに任せる。

## 13. リポジトリ境界とデプロイ経路

`nuage-cluster/nix/flake.nix` は `nuage-workspace` を外部 flake input として取り込む。

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
        │ master へ push → system.autoUpgrade
        ▼
autopilot-server 上で稼働
```

### 反映手順

2 リポジトリにまたがるため順序を守る必要がある。

1. `nuage-workspace` を push
2. `nuage-cluster/nix` で `nix flake update nuage-workspace`
3. `nuage-cluster` を push
4. `sudo nixos-rebuild switch --refresh --flake "https://github.com/k-wa-wa/nuage-cluster/archive/master.tar.gz?dir=nix#autopilot-server"`

`--refresh` は必須である。Nix は tarball を `tarball-ttl`（既定 1 時間）の間キャッシュ
するため、付けないと push 直後でも古い master を掴む。

## 14. ディレクトリ構成

```
nuage-workspace/
├── flake.nix                       # packages を export
└── autopilot/
    ├── DESIGN.md                   # 本ファイル
    ├── go.mod / vendor/            # github.com/ncruces/go-sqlite3 のため vendor をコミットする
    ├── secrets.env.example
    ├── cmd/nuage-autopilot/
    │   └── main.go                 # goroutine の起動と停止のみ
    └── internal/
        ├── config/                 # フラグ・環境変数の解決
        ├── store/                  # SQLite。items / events / leases / cursors
        ├── github/                 # Issue / PR / notifications / check-runs (net/http)
        ├── ingest/                 # notifications ポーラー、resync、イベント正規化
        ├── engine/                 # 遷移表、lease、予算
        ├── prompt/                 # エージェントのプロンプト（verify は将来）
        ├── repo/                   # 対象リポジトリの clone / 更新
        ├── runner/                 # claude の起動、セッション管理
        └── daemon/                 # goroutine のループ、sd_notify、graceful shutdown
```

## 15. シークレットの取り扱い

GitHub / Claude のトークンは SOPS で配布しない。流出時の影響が大きいため、
VM 起動後に手作業で配置する運用とする。

- 配置先: `/var/lib/nuage-autopilot/secrets.env`（`User` (`nixos`) 所有 / `0600`）
- 参照方法: systemd の `EnvironmentFile`。先頭に `-` を付け、ファイル不在でも起動に失敗させない
- 書式は systemd の EnvironmentFile であり、シェルスクリプトではない

| 変数 | 用途 |
| :-- | :-- |
| `GH_TOKEN` | gh CLI / GitHub API / git push の認証 |
| `GIT_AUTHOR_NAME` / `GIT_AUTHOR_EMAIL` | 生成コミットの名義。committer にも同じ値を使う |
| `NUAGE_ALLOWED_AUTHORS` | 対象とする Issue/PR の作成者のカンマ区切りリスト |

`secrets.env` は誤コミットを防ぐため `.gitignore` に登録する。

### LLM CLI の認証は環境変数で渡さない

claude は CLI の TUI でサインインし、認証情報を実行ユーザーの HOME（`~/.claude`）に
保存する。API キーを `secrets.env` に置く必要はない。

代わりに、**人間がサインインするユーザーとサービスの実行ユーザーを一致させる**必要がある。
サインインは VM 上で 1 回だけ行う。

```bash
ssh nixos@192.168.5.241
claude   # TUI でサインイン
```

### 必須環境変数が未設定のときの挙動

`secrets.env` は手作業で配置する運用のため、VM 作成直後は存在しない。

`Restart = "always"` の下で「警告ログを出して即座に終了する」構成にすると、`RestartSec` の
間隔で再起動を繰り返すだけになる。

そこで**起動時に警告ログを出すが、プロセスは止めない**。poller/worker/resyncer の
各ループは動き続け、GitHub API を呼ぶ段になって初めてその呼び出しがエラーになる
（ログに現れる）。`secrets.env` を配置すれば、プロセスを再起動しなくても次の呼び出しから
自然に復帰できるようにする（`internal/github.Client` は `GH_TOKEN` を起動時に固定せず、
呼び出しごとに読み直せるようにする）。

### 対象アイテムの選別

- **オプトアウト方式**とする。`agent:ignore` ラベルが付いていない open な Issue/PR が対象
- `NUAGE_ALLOWED_AUTHORS` に該当しない作成者のアイテムは対象外とする
- 初回認識時は着火しない（7.6 節）

## 16. autopilot-server 側の構成（`nuage-cluster` リポジトリ）

VM は `terraform/vpc/zone-dev/autopilot-server.tf`、OS 構成は
`nix/hosts/autopilot-server/configuration.nix` で定義する。以下は構築済みである。

- `nix/flake.nix` の `inputs` に `nuage-workspace` を追加し、`nixosConfigurations.autopilot-server` を登録
- base-vm の qcow2 から起動し、cloud-init の hostname をもとに `nixos-bootstrap` が構成を自動適用
- `programs.nix-ld` を有効化（インストーラ版 claude の実行に必須）
- nameserver に lb の CoreDNS VIP `192.168.5.200` を指定。`cluster.wpc` をワイルドカードで
  解決するため、PR ごとに変わる preview のホスト名にも到達できる
- systemd サービスの `path` に `"/home/nixos/.local"` を含めて claude を PATH に通す

### unit 定義の要件

単一の常駐 service のみを定義する。timer unit は使わない（間隔はプロセス内部が持つ）。

```nix
systemd.services.nuage-autopilot = {
  description = "nuage-autopilot";
  after = [ "network-online.target" ];
  wants = [ "network-online.target" ];
  wantedBy = [ "multi-user.target" ];
  path = [ pkgs.git pkgs.gh "/home/nixos/.local" ];
  environment.NUAGE_STATE_DIR = "/var/lib/nuage-autopilot";
  serviceConfig = {
    Type = "notify";
    NotifyAccess = "main";
    WatchdogSec = "120s";
    Restart = "always";
    RestartSec = "10s";
    StateDirectory = "nuage-autopilot";
    EnvironmentFile = "-/var/lib/nuage-autopilot/secrets.env";
    TimeoutStopSec = "5m";
    ExecStart = "${lib.getExe pkg} --repos ${reposArg}";
    User = "nixos";
  };
};
```

- `Type = "notify"` + `WatchdogSec` でハング時に再起動させる。プロセスは起動完了時に
  `READY=1` を、以後 30 秒ごとに `WATCHDOG=1` を送る
- `Restart = "always"` とする。クラッシュしても lease の TTL により作業は自然に再開される
- `TimeoutStopSec` は実行中の claude に猶予を与えるため長めに取る
- `User = "nixos"`（`DynamicUser` は使わない。`git clone` と claude が固定の HOME を
  要求するため）
- `EnvironmentFile` は先頭 `-` 付き（ファイルが存在しなくても起動失敗させない）
- `path` に `pkgs.git` / `pkgs.gh` / `"/home/nixos/.local"`（claude インストーラの配置先）を含める

人手が必要な作業は `secrets.env` の配置と claude の TUI サインインのみである。

