package cycle

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/k-wa-wa/nuage-workspace/autopilot/internal/github"
	"github.com/k-wa-wa/nuage-workspace/autopilot/internal/runner"
)

// DispatcherModel は dispatcher の呼び出しに使うモデルである。判断のみで実装作業を
// 伴わないため、常に haiku を明示指定する（DESIGN.md 8章「dispatcher の契約」）。
const DispatcherModel = "claude-haiku-4-5-20251001"

// dispatchMaxAttempts は dispatcher 呼び出しの最大試行回数である。
// DESIGN.md 8章「パース失敗または不正値の場合は 1 回だけ再試行し、それでも駄目なら
// 何もせず終了する」に対応する（初回 + 再試行 1 回 = 2）。
const dispatchMaxAttempts = 2

// Worker* は dispatcher が選択できる worker の識別子である（DESIGN.md 8章「worker」）。
const (
	WorkerSpec   = "spec"
	WorkerDev    = "dev"
	WorkerReview = "review"
	WorkerQA     = "qa"
	WorkerNone   = "none"
)

var validWorkers = map[string]bool{
	WorkerSpec:   true,
	WorkerDev:    true,
	WorkerReview: true,
	WorkerQA:     true,
	WorkerNone:   true,
}

// bodyPreviewLimit は DispatchCandidate.Body に含める Issue/PR 本文の最大文字数である。
// DESIGN.md 8章「dispatcher へは Issue/PR の本文と直近のコメント履歴を渡す」に対応する。
// 「仕様が固まっているか」を判断するには本文の要点（背景・要件・受け入れ基準）が
// 読める程度の分量が要る。2000 文字あれば typical な Issue 本文はほぼ全文入り、
// 極端に長い設計ドキュメント級の本文でも冒頭の要旨は収まるという想定で決めた値である。
// clone しない dispatcher にとってこれが唯一の一次情報源になるため、コメントの
// プレビューより大きく取っている。
const bodyPreviewLimit = 2000

// commentPreviewLimit は DispatchCommentSummary.Preview に含める本文の最大文字数である。
// dispatcher は clone せずメタデータのみで判断するが、直近コメントの本文が短すぎると
// 「レビュー合格/不合格」「仕様の承認/差し戻し」といった状態を判別しきれない
// （旧 200 文字では bot のレビュー結果コメントの結論部分が切れて読めないことがあった）。
// 600 文字であれば、レビュー結果や質問への回答といったコメントの要点まで大抵は収まる。
const commentPreviewLimit = 600

// recentCommentLimit は DispatchCandidate に含める直近コメントの件数である。
// 5 件では「spec への質問 → 回答 → 再質問」のような往復が数往復続いただけで
// 一番古い文脈（最初の指摘や承認）が切り捨てられてしまうことがあったため、8 件に増やした。
// LoopLimit（既定 5）より大きくすることで、ループ上限判定の対象になり得る
// bot コメントの並びも dispatcher からある程度見える範囲に収める狙いもある。
const recentCommentLimit = 8

// DispatchCandidate は dispatcher に渡す 1 アイテム分のメタデータである。
// DESIGN.md 8章「dispatcher へは Issue/PR の本文と直近のコメント履歴を渡す」に従い、
// 本文（Body）を含める。ただし clone はしないため、本文・コメントとも文字数で
// 切り詰めたうえで渡す（bodyPreviewLimit / commentPreviewLimit）。
type DispatchCandidate struct {
	Kind      itemKind
	Number    int
	Title     string
	Author    string
	UpdatedAt time.Time
	Labels    []string

	// Body は Issue/PR 本文を bodyPreviewLimit で切り詰めたものである。切り詰めが
	// 発生した場合は truncateRunes が末尾に "…" を付与するため、dispatcher 側でも
	// 「本文の続きが省略されている」と認識できる。
	Body           string
	RecentComments []DispatchCommentSummary
}

// DispatchCommentSummary は DispatchCandidate に含める直近コメント 1 件分の要約である。
type DispatchCommentSummary struct {
	Author    string
	IsBot     bool
	CreatedAt time.Time
	Preview   string
}

// Decision は dispatcher の出力である（DESIGN.md 8章の JSON 契約に対応する）。
type Decision struct {
	Number int    `json:"number"`
	Kind   string `json:"kind"`
	Worker string `json:"worker"`
	Reason string `json:"reason"`
}

// Dispatcher は 1 サイクルにつき 1 回、候補アイテムの中からどれをどの worker に
// 渡すかを決める処理の抽象である。cycle.Run はこのインターフェース越しにのみ
// dispatcher を呼ぶため、cycle パッケージのテストでは実際の claude を起動しない
// フェイク実装に差し替えられる。
//
// 戻り値の bool は「実行可能な決定が得られたかどうか」を表す。false の場合
// （dispatcher が worker=none と判断した場合、またはパース失敗・不正値が
// リトライしても解消しなかった場合の両方を含む）、呼び出し側は何もせず
// このサイクルを終える。
type Dispatcher interface {
	Dispatch(ctx context.Context, repo string, candidates []DispatchCandidate) (Decision, bool, error)
}

// DefaultDispatcher は claude (haiku) を使う本番用の Dispatcher 実装である。
type DefaultDispatcher struct {
	// StateDir は claude を起動する作業ディレクトリである。dispatcher は clone を
	// 伴わないため、対象リポジトリの clone 先である必要はなく、存在してさえいれば
	// よい（stateDir 自体を指定する想定）。
	StateDir string

	// Logger は nil の場合 slog.Default() を使う。
	Logger *slog.Logger

	// Command は runner.Run に渡す実行ファイル名/パスの上書き。テスト用。
	Command string
}

// Dispatch は DefaultDispatcher の Dispatcher 実装である。
func (d *DefaultDispatcher) Dispatch(ctx context.Context, repo string, candidates []DispatchCandidate) (Decision, bool, error) {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}

	prompt := buildDispatchPrompt(repo, candidates)

	var lastErr error
	for attempt := 1; attempt <= dispatchMaxAttempts; attempt++ {
		decision, err := d.callOnce(ctx, prompt, logger)
		if err == nil {
			if verr := validateDecision(decision, candidates); verr == nil {
				if decision.Worker == WorkerNone {
					logger.Info("dispatcher decided there is nothing to dispatch this cycle",
						"repo", repo, "reason", decision.Reason)
					return Decision{}, false, nil
				}
				return decision, true, nil
			} else {
				lastErr = verr
			}
		} else {
			lastErr = err
		}
		logger.Warn("dispatcher attempt did not produce a usable decision",
			"repo", repo, "attempt", attempt, "max_attempts", dispatchMaxAttempts, "error", lastErr.Error())
	}

	// DESIGN.md 8章: パース失敗・不正値が再試行しても解消しない場合、何もせず終了する。
	// アイテムにラベルを付けないため、次サイクルで再度試行される。
	logger.Warn("dispatcher failed after retry; skipping this cycle", "repo", repo, "error", lastErr.Error())
	return Decision{}, false, nil
}

// callOnce は claude (haiku) を 1 回起動し、応答を Decision にデコードする。
// 検証（validateDecision）は呼び出し元の責務であり、ここでは JSON としての妥当性
// のみを扱う。
func (d *DefaultDispatcher) callOnce(ctx context.Context, prompt string, logger *slog.Logger) (Decision, error) {
	result, err := runner.Run(ctx, runner.Options{
		Command:   d.Command,
		WorkDir:   d.StateDir,
		Prompt:    prompt,
		Model:     DispatcherModel,
		ExtraArgs: []string{"--output-format", "json", "--json-schema", dispatchJSONSchema},
		Logger:    logger,
	})
	if err != nil {
		return Decision{}, fmt.Errorf("dispatcher: run claude: %w", err)
	}
	if !result.Success {
		return Decision{}, fmt.Errorf("dispatcher: claude exited with non-zero status %d", result.ExitCode)
	}

	// claude --output-format json のラッパ。"result" は応答本文の文字列であり、
	// --json-schema を指定した場合は加えて "structured_output" にスキーマに沿った
	// JSON がそのまま入る（`claude --help` で確認し、実際に手元で検証済み）。
	// このラッパ自体と、dispatcher のプロンプトに出力させる JSON 本体（Decision）を
	// 混同しないよう、必ず structured_output 経由でデコードする。
	var wrapper struct {
		IsError           bool            `json:"is_error"`
		Result            string          `json:"result"`
		StructuredOutput  json.RawMessage `json:"structured_output"`
		APIErrorStatus    *int            `json:"api_error_status"`
		PermissionDenials json.RawMessage `json:"permission_denials"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &wrapper); err != nil {
		return Decision{}, fmt.Errorf("dispatcher: decode claude output wrapper: %w", err)
	}
	if wrapper.IsError {
		return Decision{}, fmt.Errorf("dispatcher: claude reported is_error=true: %s", wrapper.Result)
	}
	if len(wrapper.StructuredOutput) == 0 {
		return Decision{}, fmt.Errorf("dispatcher: claude response is missing structured_output")
	}

	var decision Decision
	if err := json.Unmarshal(wrapper.StructuredOutput, &decision); err != nil {
		return Decision{}, fmt.Errorf("dispatcher: decode structured_output: %w", err)
	}
	return decision, nil
}

// dispatchJSONSchema は --json-schema に渡す JSON Schema である。
// number/kind は worker="none" のときは無視されるため必須にしていない
// （必須にすると worker="none" のときも意味のない値を強制することになるため）。
// worker と reason は常に意味を持つため必須とする。
const dispatchJSONSchema = `{"type":"object","properties":{"number":{"type":"integer"},"kind":{"type":"string","enum":["issue","pull_request"]},"worker":{"type":"string","enum":["spec","dev","review","qa","none"]},"reason":{"type":"string"}},"required":["worker","reason"],"additionalProperties":false}`

// validateDecision は Decision が DESIGN.md 8章の契約を満たしているかを検証する。
//   - worker は spec/dev/review/qa/none のいずれかであること
//   - worker が none の場合、number/kind は検証しない（判断の対象が無いという結果自体が
//     有効な決定であるため）
//   - worker が none 以外の場合、number と kind が手順1で取得した候補集合に含まれ、
//     かつ worker がその kind に対して選択可能であること
func validateDecision(d Decision, candidates []DispatchCandidate) error {
	if !validWorkers[d.Worker] {
		return fmt.Errorf("dispatcher: invalid worker %q", d.Worker)
	}
	if d.Worker == WorkerNone {
		return nil
	}

	for _, c := range candidates {
		if string(c.Kind) == d.Kind && c.Number == d.Number {
			if !workerSupportsKind(d.Worker, c.Kind) {
				return fmt.Errorf("dispatcher: worker %q is not valid for kind %q", d.Worker, d.Kind)
			}
			return nil
		}
	}
	return fmt.Errorf("dispatcher: item kind=%q number=%d is not part of the candidate set", d.Kind, d.Number)
}

// workerSupportsKind は worker が kind のアイテムに対して選択可能かどうかを返す。
// DESIGN.md 8章の worker 一覧（spec は仕様定義=Issue、review/qa は PR 向けの検証）に
// 基づく制約であり、DESIGN.md に明文化されているわけではないが、number/kind の
// 整合性チェックの自然な延長として追加した（詳細は実装報告を参照）。
func workerSupportsKind(worker string, kind itemKind) bool {
	switch worker {
	case WorkerSpec:
		return kind == kindIssue
	case WorkerDev:
		return kind == kindIssue || kind == kindPullRequest
	case WorkerReview, WorkerQA:
		return kind == kindPullRequest
	default:
		return false
	}
}

// buildDispatchCandidates は cycle.Run が組み立てた候補アイテムと、事前に取得済みの
// コメント一覧（ループ上限判定と使い回す。二重に GitHub API を叩かない）から、
// dispatcher に渡す DispatchCandidate のスライスを組み立てる。
func buildDispatchCandidates(items []Item, commentsByNumber map[int][]github.Comment, botLogin string) []DispatchCandidate {
	out := make([]DispatchCandidate, 0, len(items))
	for _, it := range items {
		comments := commentsByNumber[it.Number]
		sorted := make([]github.Comment, len(comments))
		copy(sorted, comments)
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
		})
		if len(sorted) > recentCommentLimit {
			sorted = sorted[:recentCommentLimit]
		}

		summaries := make([]DispatchCommentSummary, 0, len(sorted))
		for _, c := range sorted {
			summaries = append(summaries, DispatchCommentSummary{
				Author:    c.User.Login,
				IsBot:     isBotComment(c, botLogin),
				CreatedAt: c.CreatedAt,
				Preview:   truncateRunes(c.Body, commentPreviewLimit),
			})
		}

		out = append(out, DispatchCandidate{
			Kind:           it.Kind,
			Number:         it.Number,
			Title:          it.Title,
			Author:         it.Author,
			UpdatedAt:      it.UpdatedAt,
			Labels:         it.Labels,
			Body:           truncateRunes(it.Body, bodyPreviewLimit),
			RecentComments: summaries,
		})
	}
	return out
}

// truncateRunes は s を最大 n rune に切り詰める。マルチバイト文字（日本語コメント等）を
// 途中で破壊しないよう rune 単位で扱う。
// 切り詰めが発生した場合は末尾に "…" を付与する。これにより dispatcher 側は
// 「情報が途中で切られている＝断定材料として不十分かもしれない」と認識できる
// （切り詰めた事実を伝えないと、欠けた情報から誤って断定するおそれがあるため）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// buildDispatchPrompt は候補一覧から dispatcher 向けの日本語プロンプトを組み立てる。
func buildDispatchPrompt(repo string, candidates []DispatchCandidate) string {
	var b strings.Builder

	fmt.Fprintf(&b, "あなたは対象リポジトリ「%s」の dispatcher である。\n", repo)
	b.WriteString("以下の候補一覧の中から、次にどのアイテムをどの worker に渡すべきかを判断する。\n")
	b.WriteString("あなた自身は実装や検証を行わない。判断のみを行い、指定された JSON 形式で出力する。\n\n")

	b.WriteString("## worker の役割\n")
	b.WriteString("- spec: 要求を PRD と受け入れ基準に落とす。Issue にのみ選べる。\n")
	b.WriteString("- dev: ブランチを切り実装し、テスト通過まで自己修復して PR を作成する。Issue/PR どちらにも選べる。\n")
	b.WriteString("- review: バグ・セキュリティ・性能に加え、設計規約・影響範囲を検証する。PR にのみ選べる。\n")
	b.WriteString("- qa: preview 環境に対する E2E を含む最終検証を行う。PR にのみ選べる。\n")
	b.WriteString("- none: 今回着手すべき候補が無いと判断した場合に選ぶ。\n\n")

	b.WriteString("## 候補一覧\n")
	if len(candidates) == 0 {
		b.WriteString("(候補は無い)\n")
	}
	for _, c := range candidates {
		fmt.Fprintf(&b, "- kind=%s number=%d title=%q author=%s updated_at=%s labels=%v\n",
			c.Kind, c.Number, c.Title, c.Author, c.UpdatedAt.UTC().Format(time.RFC3339), c.Labels)
		if c.Body == "" {
			b.WriteString("  本文: (無し)\n")
		} else {
			fmt.Fprintf(&b, "  本文: %q\n", c.Body)
		}
		if len(c.RecentComments) == 0 {
			b.WriteString("  直近コメント: (無し)\n")
			continue
		}
		b.WriteString("  直近コメント（新しい順）:\n")
		for _, cm := range c.RecentComments {
			kind := "human"
			if cm.IsBot {
				kind = "bot"
			}
			fmt.Fprintf(&b, "    - %s (%s) %s: %q\n",
				cm.Author, kind, cm.CreatedAt.UTC().Format(time.RFC3339), cm.Preview)
		}
	}

	b.WriteString("\n## 本文・コメントの読み方\n")
	b.WriteString("- 本文は Issue/PR 作成時点の内容であり、「要求・仕様がどこまで固まっているか」（背景・要件・受け入れ基準が書かれているか、曖昧なままか）を判断する主な材料である。\n")
	b.WriteString("- コメントは新しい順に並んでいる。コメントの 1 行目が \"<!-- nuage-autopilot worker=… status=… -->\" 形式の状態行である場合、それが最も信頼できる情報である。散文の解釈より状態行を優先すること。\n")
	b.WriteString("- 状態行の読み替え: status=passed の review の次は qa、status=failed の review/qa の次は dev、status=done の spec/dev の次はそれぞれ dev/review が基本である。\n")
	b.WriteString("- 本文・コメント本文の末尾が \"…\" で終わっている場合、文字数制限により切り詰められているため、末尾が \"…\" のときは文面から読み取れる範囲で判断すること。\n\n")

	b.WriteString("## 判断の指針\n")
	b.WriteString("- 候補一覧には、agent: 接頭辞のラベルが付いていない open な Issue/PR のみが含まれている。\n")
	b.WriteString("- 直近のコメントが人間からのものであれば、その内容を踏まえて次の worker を判断する。\n")
	b.WriteString("- 直近のコメントが nuage-autopilot 自身 (bot) からのものであれば、上記の状態行およびコメント内容から次に必要な worker を決定する。\n")
	b.WriteString("- コメントが無い、または着手されていない新規の Issue には spec を、まだレビューを受けていない新規の PR には review を割り当てるのが基本である。\n")
	b.WriteString("- 判断がつかない場合や、着手すべき候補が無い場合は、無理に選ばず worker を \"none\" とすること。\n\n")

	b.WriteString("## 出力形式\n")
	b.WriteString("指定された JSON スキーマに従って構造化出力を返すこと。散文や補足を出力に含めないこと。\n")
	b.WriteString("- worker が \"none\" 以外の場合: number と kind は候補一覧に実在する組み合わせでなければならない。\n")
	b.WriteString("- worker が \"none\" の場合: number と kind は省略してよい。\n")
	b.WriteString("- reason には、どのコメント・状態行を根拠にその worker を選んだのかを簡潔に書くこと。\n")

	return b.String()
}
