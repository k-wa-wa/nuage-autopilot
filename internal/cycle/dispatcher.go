package cycle

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"autopilot/internal/github"
	"autopilot/internal/report"
	"autopilot/internal/runner"
)

// DispatcherModel は dispatcher の呼び出しに使うモデルである。判断のみで実装作業を
// 伴わないため、常に haiku を明示指定する。
const DispatcherModel = "claude-haiku-4-5-20251001"

// dispatchMaxAttempts は dispatcher 呼び出しの最大試行回数である。
// パース失敗または不正値の場合は 1 回だけ再試行し、それでも駄目なら何もせず終了する
// （初回 + 再試行 1 回 = 2）。
const dispatchMaxAttempts = 2

// Worker* は dispatcher が選択できる worker の識別子である。
// work/verify は internal/report が定義する状態行の worker= 値と共通であり、
// dispatcher の決定と worker の実行結果を同じ語彙で扱えるようにするため report
// パッケージの定数をそのまま再エクスポートする。none は「今回は何もしない」を表す
// dispatcher 固有の値であり、状態行には現れない。
const (
	WorkerWork   = report.WorkerWork
	WorkerVerify = report.WorkerVerify
	WorkerNone   = "none"
)

var validWorkers = map[string]bool{
	WorkerWork:   true,
	WorkerVerify: true,
	WorkerNone:   true,
}

// bodyPreviewLimit は DispatchCandidate.Body に含める Issue/PR 本文の最大文字数である。
// dispatcher は「どのアイテムを処理するか」の判断材料としてのみ本文を必要とし、
// 実際の実装・検証は worker が別途本文全文を読んで行うため、要点が読める程度の
// 分量で足りる。
const bodyPreviewLimit = 2000

// commentPreviewLimit は DispatchCommentSummary.Preview に含める本文の最大文字数である。
const commentPreviewLimit = 600

// recentCommentLimit は DispatchCandidate に含める直近コメントの件数である。
const recentCommentLimit = 8

// DispatchCandidate は dispatcher に渡す 1 アイテム分のメタデータである。
//
// cycle.Run が遷移表（transition.go）で "ask" と判定した候補のみがここに渡る。
// つまり CIStatus/Draft/RelatedPRs といった機械的な状態は既に遷移表で評価済みであり、
// dispatcher にとっては「なぜ人間の判断を仰ぐ必要があるか」を理解するための
// 参考情報にすぎない（決定ルールとしては使わない）。
type DispatchCandidate struct {
	Kind      itemKind
	Number    int
	Title     string
	Author    string
	UpdatedAt time.Time
	Labels    []string

	// CIStatus は PR コミットに対する CI チェックランの状態 ("success", "failure", "pending", "none") である。
	// 参考情報。
	CIStatus string

	// Draft は PR が Draft 状態かどうかを表す。参考情報。
	Draft bool

	// RelatedPRs はこの Issue に対するオープンな PR 番号の一覧である（Issue のみ）。参考情報。
	RelatedPRs []int

	// PendingReason は遷移表がこの候補を "ask" と判定した理由である
	// （例: "human commented after the last status line"）。
	PendingReason string

	// Body は Issue/PR 本文を bodyPreviewLimit で切り詰めたものである。
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

// Decision は dispatcher の出力である。
type Decision struct {
	Number int    `json:"number"`
	Kind   string `json:"kind"`
	Worker string `json:"worker"`
	Reason string `json:"reason"`
}

// Dispatcher は、遷移表だけでは機械的に決められなかった候補（"ask" と判定された
// 候補）の中から、直近の人間コメントの意図を汲んで次のアクションを決める処理の
// 抽象である。cycle.Run はこのインターフェース越しにのみ dispatcher を呼ぶため、
// cycle パッケージのテストでは実際の claude を起動しないフェイク実装に差し替えられる。
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

	basePrompt := buildDispatchPrompt(repo, candidates)

	var lastErr error
	for attempt := 1; attempt <= dispatchMaxAttempts; attempt++ {
		currentPrompt := basePrompt
		if attempt > 1 && lastErr != nil {
			currentPrompt += fmt.Sprintf("\n\n## 前回の失敗理由（必ず修正してください）\n前回の出力は以下のエラーにより却下されました:\n%s\n\n候補一覧とルールを再度確認し、正しい形式で出力してください。\n", lastErr.Error())
		}

		decision, err := d.callOnce(ctx, currentPrompt, logger)
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

	// パース失敗・不正値が再試行しても解消しない場合、何もせず終了する。
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
		ExtraArgs: []string{"--output-format", "json", "--json-schema", dispatchJSONSchema, "--tools", ""},
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
	// JSON がそのまま入る。このラッパ自体と、dispatcher のプロンプトに出力させる
	// JSON 本体（Decision）を混同しないよう、必ず structured_output 経由でデコードする。
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
// number/kind は worker="none" のときは無視されるため必須にしていない。
const dispatchJSONSchema = `{"type":"object","properties":{"number":{"type":"integer"},"kind":{"type":"string","enum":["issue","pull_request"]},"worker":{"type":"string","enum":["work","verify","none"]},"reason":{"type":"string"}},"required":["worker","reason"],"additionalProperties":false}`

// validateDecision は Decision が dispatcher の契約を満たしているかを検証する。
//   - worker は work/verify/none のいずれかであること
//   - worker が none の場合、number/kind は検証しない
//   - worker が none 以外の場合、number と kind が候補集合に含まれ、
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
// work は Issue にも PR にも選べるが、verify はコードを検証するフェーズであり
// PR にしか意味を持たない。
func workerSupportsKind(worker string, kind itemKind) bool {
	switch worker {
	case WorkerWork:
		return kind == kindIssue || kind == kindPullRequest
	case WorkerVerify:
		return kind == kindPullRequest
	default:
		return false
	}
}

var reRelatedIssue = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)`)

func extractRelatedIssueNumbers(body string) []int {
	matches := reRelatedIssue.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}
	var res []int
	seen := make(map[int]bool)
	for _, m := range matches {
		if len(m) > 1 {
			if num, err := strconv.Atoi(m[1]); err == nil && !seen[num] {
				seen[num] = true
				res = append(res, num)
			}
		}
	}
	return res
}

// buildIssuePRLinks は全 open アイテム（全 Issue + 全 PR）を走査し、
// PR の Body から関連 Issue 番号を抽出して Issue 番号 -> 関連 PR 番号リストのマップを構築する。
// フィルタリング（ラベル除外等）前の全アイテムから計算することで、除外された PR でも
// 関連 Issue への紐付けが維持されるようにする。
func buildIssuePRLinks(items []Item) map[int][]int {
	issueToPRs := make(map[int][]int)
	for _, it := range items {
		if it.Kind == kindPullRequest {
			for _, issueNum := range extractRelatedIssueNumbers(it.Body) {
				issueToPRs[issueNum] = append(issueToPRs[issueNum], it.Number)
			}
		}
	}
	return issueToPRs
}

// buildDispatchCandidates は cycle.Run が "ask" と判定した候補アイテムと、事前計算
// された Issue-PR 紐付けマップ、コメント一覧、判定理由から dispatcher に渡す
// DispatchCandidate のスライスを組み立てる。reasons が nil または該当エントリが
// 無い場合、PendingReason は空文字列のままとする。
func buildDispatchCandidates(items []Item, reasons map[int]string, commentsByNumber map[int][]github.Comment, botLogin string, relatedPRs map[int][]int) []DispatchCandidate {
	issueToPRs := relatedPRs

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
			CIStatus:       it.CIStatus,
			Draft:          it.Draft,
			RelatedPRs:     issueToPRs[it.Number],
			PendingReason:  reasons[it.Number],
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
//
// cycle.Run はここに渡す候補を「遷移表だけでは機械的に決められなかったもの」に
// 絞り込み済みである。CI 状態や related_open_prs による自動ルーティングは
// 遷移表（transition.go）側の責務であり、dispatcher に判断させるのは
// 「直近の人間コメントの意図が work・verify・none のどれに当たるか」の 1 点のみである。
func buildDispatchPrompt(repo string, candidates []DispatchCandidate) string {
	var b strings.Builder

	fmt.Fprintf(&b, "あなたは対象リポジトリ「%s」の dispatcher である。\n", repo)
	b.WriteString("以下の候補はいずれも、直近の人間コメントの意図が機械的なルールだけでは判断できないため、あなたの判断を必要としている。\n")
	b.WriteString("各候補について、本文および直近コメントを読み、次に何をすべきかを判断する。あなた自身は実装や検証を行わない。判断のみを行い、指定された JSON 形式で出力する。\n\n")

	b.WriteString("## 選択肢\n")
	b.WriteString("- work: 実装の追加・修正が必要だと判断した場合。Issue/PR どちらにも選べる。\n")
	b.WriteString("- verify: コードは変更せず再検証すればよいと判断した場合。PR にのみ選べる。\n")
	b.WriteString("- none: 対応不要（承認・雑談・様子見のコメント等）と判断した場合。\n\n")

	b.WriteString("## 候補一覧\n")
	if len(candidates) == 0 {
		b.WriteString("(候補は無い)\n")
	}
	for _, c := range candidates {
		extraInfo := ""
		if c.Kind == kindPullRequest {
			if c.CIStatus != "" && c.CIStatus != "none" {
				extraInfo += fmt.Sprintf(" ci_status=%s", c.CIStatus)
			}
			if c.Draft {
				extraInfo += " draft=true"
			}
		} else if c.Kind == kindIssue && len(c.RelatedPRs) > 0 {
			extraInfo += fmt.Sprintf(" related_open_prs=%v", c.RelatedPRs)
		}
		fmt.Fprintf(&b, "- kind=%s number=%d title=%q author=%s updated_at=%s labels=%v%s\n",
			c.Kind, c.Number, c.Title, c.Author, c.UpdatedAt.UTC().Format(time.RFC3339), c.Labels, extraInfo)
		if c.PendingReason != "" {
			fmt.Fprintf(&b, "  保留理由: %s\n", c.PendingReason)
		}
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

	b.WriteString("\n## 読み方\n")
	b.WriteString("- 本文は Issue/PR 作成時点の内容である。\n")
	b.WriteString("- コメントは新しい順に並んでいる。1 行目が \"<!-- nuage-autopilot worker=… status=… -->\" 形式の状態行であるコメントは nuage-autopilot 自身の投稿であり、それより新しい人間のコメントが判断対象である。\n")
	b.WriteString("- 本文・コメント本文の末尾が \"…\" の場合、文字数制限により切り詰められている。\n\n")

	b.WriteString("## 判断の指針\n")
	b.WriteString("- 直近の人間コメントの内容から、修正の指示（work）・再検証の依頼（verify）・対応不要（none）のいずれかを判断すること。\n")
	b.WriteString("- 判断がつかない場合は、無理に選ばず worker を \"none\" とすること。\n\n")

	b.WriteString("## 出力形式\n")
	b.WriteString("指定された JSON スキーマに従って構造化出力を返すこと。散文や補足を出力に含めないこと。\n")
	b.WriteString("- worker が \"none\" 以外の場合: number と kind は候補一覧に実在する組み合わせでなければならない。\n")
	b.WriteString("- worker が \"none\" の場合: number と kind は省略してよい。\n")
	b.WriteString("- reason には、どのコメントを根拠にその判断をしたのかを簡潔に書くこと。\n")

	return b.String()
}
