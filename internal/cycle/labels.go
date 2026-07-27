package cycle

// LabelPrefix は autopilot が管理するラベルの接頭辞である。
const LabelPrefix = "agent:"

// 以下、DESIGN.md 8章「ラベル状態機械」に定義されたラベル一覧。
const (
	LabelSpec           = "agent:spec"
	LabelDev            = "agent:dev"
	LabelReviewGeneral  = "agent:review-general"
	LabelReviewSemantic = "agent:review-semantic"
	LabelQA             = "agent:qa"
	LabelWait           = "agent:wait"
	LabelTriage         = "agent:triage"
)

// activePhases は LLM の実行を伴うフェーズラベルを優先順に列挙したものである。
// DESIGN.md のフロー（spec → dev → review-general / review-semantic → qa）に
// 沿った順序にしてある。1 つの Issue/PR に複数のフェーズラベルが同時に付く状態は
// 本来ないはずだが、もし付いていた場合はこの順序で最初に見つかったものを採用する
// （classifyLabels 参照）。
var activePhases = []string{
	LabelSpec,
	LabelDev,
	LabelReviewGeneral,
	LabelReviewSemantic,
	LabelQA,
}

// LabelState はある Issue/PR のラベルから読み取れる状態機械上の状態を表す。
type LabelState struct {
	// Phase は現在のアクティブフェーズラベル（LabelSpec 等）。
	// 該当するラベルが 1 つもない場合は空文字列。LabelTriage の場合も Phase には
	// 入れず、Triage フィールドで表現する。
	Phase string

	// Waiting は LabelWait が付与されているかどうか。
	Waiting bool

	// Triage は LabelTriage が付与されているかどうか。
	Triage bool

	// HasAnyAgentLabel は agent: 接頭辞のラベルが 1 つでも付いているかどうか。
	// 「ラベルが 1 つも付いていない Issue には agent:spec を付与する」判定に使う。
	HasAnyAgentLabel bool

	// Ambiguous は activePhases のうち複数が同時に付いている（本来ありえない）
	// 異常な状態を検知した場合に true になる。呼び出し側はログに警告を残すべきである。
	Ambiguous bool
}

// classifyLabels は labels（Issue/PR に付与されている生のラベル名一覧）から
// LabelState を組み立てる。
func classifyLabels(labels []string) LabelState {
	set := make(map[string]bool, len(labels))
	for _, l := range labels {
		set[l] = true
	}

	state := LabelState{
		Waiting: set[LabelWait],
		Triage:  set[LabelTriage],
	}

	for _, l := range labels {
		if len(l) >= len(LabelPrefix) && l[:len(LabelPrefix)] == LabelPrefix {
			state.HasAnyAgentLabel = true
			break
		}
	}

	for _, phase := range activePhases {
		if set[phase] {
			if state.Phase != "" {
				state.Ambiguous = true
				continue
			}
			state.Phase = phase
		}
	}

	return state
}
