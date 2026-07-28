package store

import "time"

// Kind は items.kind に入る値である。
type Kind string

const (
	KindIssue       Kind = "issue"
	KindPullRequest Kind = "pull_request"
)

// Phase は items.phase に入る値である。DESIGN.md 6.2 節を参照。
//
// phase と lease は直交する概念である。「今誰かが作業中か」は Lease が表し、
// phase はストーリー上どこまで進んだかを表す永続的な状態である。したがって
// "working" に相当する phase は存在しない。プロセスがクラッシュして lease が
// 失効しても phase はそのまま残り、次の実行が自然に再開できる。
type Phase string

const (
	// PhaseNew は認識したが未着手であることを表す。
	PhaseNew Phase = "new"

	// PhaseAwaitingAnswer はエージェントが質問し、人間の回答を待っていることを表す。
	// ラベルではなく phase として持つことで、人間が回答すればそのコメントがイベントに
	// なり、そのまま次に進む（人間が剥がすべきラベルは存在しない）。
	PhaseAwaitingAnswer Phase = "awaiting_answer"

	// PhaseInReview は PR が存在し、CI・検証・修正を反復している状態を表す。
	PhaseInReview Phase = "in_review"

	// PhaseReady は実装が済み CI も緑で、人間のマージを待っている状態を表す。
	// 将来 verify を追加した場合、verify 合格もこの phase への遷移条件に加わる
	// （DESIGN.md 8.4 節）。
	PhaseReady Phase = "ready"

	// PhaseBlocked は人間の判断が必要で中断していることを表す。
	PhaseBlocked Phase = "blocked"

	// PhaseDelegated はサブ Issue に分割済みで、親自身は直接実装しないことを表す。
	// 全子が完了するまでこの phase から出ない（二重実装を構造的に防ぐ）。
	PhaseDelegated Phase = "delegated"

	// PhaseDone は close / merge 済みであることを表す。
	PhaseDone Phase = "done"
)

// Item は 1 つの Issue/PR の永続状態である（DESIGN.md 6.3 節）。
type Item struct {
	ID         int64
	Repo       string
	Number     int
	Kind       Kind
	Phase      Phase
	ParentID   *int64
	SessionID  string
	HeadSHA    string
	CostUSD    float64
	Runs       int
	LastSeenAt *time.Time
	UpdatedAt  time.Time
}

// Event は取り込んだ 1 件の出来事である。processed_at が NULL のものが未処理キューを成す。
type Event struct {
	ID          int64
	DedupKey    string
	ItemID      int64
	Type        string
	Actor       string
	Body        string
	CreatedAt   time.Time
	ProcessedAt *time.Time
}

// Lease は 1 アイテムに対する排他制御である。TTL 付きであり、保持者がクラッシュ
// しても expires_at を過ぎれば自動的に回収される。
type Lease struct {
	ItemID    int64
	Holder    string
	ExpiresAt time.Time
}

// Cursor はイベント取り込み source（"notifications" 等）ごとの読み取り位置である。
type Cursor struct {
	Source       string
	ETag         string
	LastModified string
	Since        string
	PolledAt     *time.Time
}
