package github

import "time"

// Author は Issue/PR/コメントの作成者を表す。
type Author struct {
	// Login は GitHub のユーザー名である。
	Login string `json:"login"`

	// Type は "User" / "Bot" / "Organization" のいずれかである。
	// GitHub App 経由の投稿は "Bot" になるが、個人の Personal Access Token を
	// 自動化用アカウントとして使う運用では "User" のままになる点に注意する。
	// そのため cycle パッケージでは Type だけでなく認証ユーザーの Login とも
	// 突き合わせて bot 判定を行う。
	Type string `json:"type"`
}

// label は GitHub API のラベル表現をデコードするための内部型である。
// nuage-autopilot はラベル名のみを扱うため、公開型（Issue/PullRequest）では
// []string に変換して保持する。
type label struct {
	Name string `json:"name"`
}

func labelNames(labels []label) []string {
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	return names
}

// rawIssue は GET /repos/{repo}/issues のレスポンス要素をそのままデコードするための型。
// PullRequest フィールドの有無で Issue と PR を判別する（GitHub の Issue API は
// PR も一緒に返すため）。
type rawIssue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	State       string    `json:"state"`
	Body        string    `json:"body"`
	Labels      []label   `json:"labels"`
	User        Author    `json:"user"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	PullRequest *struct{} `json:"pull_request,omitempty"`
}

// Issue は nuage-autopilot が扱う Issue（PR を含まない）を表す。
//
// Body は GET /repos/{repo}/issues の一覧レスポンスに元々含まれている
// フィールドであり、取得のために追加の API 呼び出しは発生しない
// （dispatcher に本文を渡す DESIGN.md 8章の要求はこのフィールド経由で満たす）。
type Issue struct {
	Number    int
	Title     string
	State     string
	Body      string
	Labels    []string
	User      Author
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (r rawIssue) isPullRequest() bool {
	return r.PullRequest != nil
}

func (r rawIssue) toIssue() Issue {
	return Issue{
		Number:    r.Number,
		Title:     r.Title,
		State:     r.State,
		Body:      r.Body,
		Labels:    labelNames(r.Labels),
		User:      r.User,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
}

// rawPullRequest は GET /repos/{repo}/pulls のレスポンス要素をデコードするための型。
type rawPullRequest struct {
	Number int     `json:"number"`
	Title  string  `json:"title"`
	State  string  `json:"state"`
	Body   string  `json:"body"`
	Labels []label `json:"labels"`
	User   Author  `json:"user"`
	Draft  bool    `json:"draft"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PullRequest は nuage-autopilot が扱う PR を表す。
type PullRequest struct {
	Number    int
	Title     string
	State     string
	Body      string
	Labels    []string
	User      Author
	Draft     bool
	HeadSHA   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (r rawPullRequest) toPullRequest() PullRequest {
	return PullRequest{
		Number:    r.Number,
		Title:     r.Title,
		State:     r.State,
		Body:      r.Body,
		Labels:    labelNames(r.Labels),
		User:      r.User,
		Draft:     r.Draft,
		HeadSHA:   r.Head.SHA,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
}

// CheckRun は GitHub の CI チェックラン情報をデコードするための型。
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // "queued", "in_progress", "completed"
	Conclusion string `json:"conclusion"` // "success", "failure", "neutral", etc.
}

// CheckRunsResponse は GET /repos/{repo}/commits/{ref}/check-runs のレスポンス型。
type CheckRunsResponse struct {
	TotalCount int        `json:"total_count"`
	CheckRuns  []CheckRun `json:"check_runs"`
}

// CommentKind は Comment がどの GitHub API 由来かを区別する。
// 通知取り込み（internal/ingest）が events.type を "commented" / "reviewed" の
// どちらにするかを判定するために使う。両者は現在の遷移表では同じ扱いだが
// （DESIGN.md 8.1 節）、由来を区別できる情報は落とさずに保持しておく。
type CommentKind string

const (
	CommentKindComment CommentKind = "comment"
	CommentKindReview  CommentKind = "review"
)

// Comment は Issue/PR に対する会話コメント、または PR レビュー本体を表す
// （PR のレビューコメント＝差分行コメントは含まない）。
type Comment struct {
	ID        int64
	Kind      CommentKind
	Body      string
	User      Author
	CreatedAt time.Time
}

type rawComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      Author    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

func (r rawComment) toComment() Comment {
	return Comment{
		ID:        r.ID,
		Kind:      CommentKindComment,
		Body:      r.Body,
		User:      r.User,
		CreatedAt: r.CreatedAt,
	}
}

type rawReview struct {
	ID          int64     `json:"id"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	User        Author    `json:"user"`
	SubmittedAt time.Time `json:"submitted_at"`
}

func (r rawReview) toComment() Comment {
	return Comment{
		ID:        r.ID,
		Kind:      CommentKindReview,
		Body:      r.Body,
		User:      r.User,
		CreatedAt: r.SubmittedAt,
	}
}

// NotificationThread は GET /notifications の 1 要素をデコードするための型である。
type NotificationThread struct {
	ID        string    `json:"id"`
	Unread    bool      `json:"unread"`
	Reason    string    `json:"reason"`
	UpdatedAt time.Time `json:"updated_at"`
	Subject   struct {
		Title string `json:"title"`
		URL   string `json:"url"`
		Type  string `json:"type"` // "Issue", "PullRequest", "Commit", "Discussion" 等
	} `json:"subject"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}
