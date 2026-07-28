package cycle

import "strings"

// LabelPrefix は autopilot が管理するラベルの接頭辞である。
//
// DESIGN.md 8章「ラベルをプログラムカウンタにしない」に従い、ラベルは「制御」の
// 役割しか持たない。対象の選別はオプトアウト方式であり、agent: 接頭辞のラベルが
// 1 つでも付いている open な Issue/PR は dispatcher の対象から外れる。
const LabelPrefix = "agent:"

const (
	// LabelRunning はロック用ラベルである。worker (claude) を起動する直前に付与し、
	// 終了時（成功・失敗いずれも）に外す。取りこぼし時の自動回収は将来課題であり、
	// 当面は人間が手動で外す運用とする（DESIGN.md 8章「将来課題: agent:running の
	// 自動回収」）。
	LabelRunning = "agent:running"

	// LabelAwaitingUserReview はゲート用ラベルである。人間の対応が必要になった時点で
	// worker 自身が gh コマンドで付与する。Go 側はこの付与そのものには関与しない
	// （関与するのはループ上限に達した場合の付与のみ。internal/cycle/looplimit.go
	// 参照）。解除は人間が行う。コメントの投稿による自動解除は行わない
	// （書きかけの返信で動き出す事故を防ぐため。DESIGN.md 8章）。
	LabelAwaitingUserReview = "agent:awaiting_user_review"
)

// hasAgentLabel は labels の中に agent: 接頭辞のラベルが 1 つでも含まれているかどうかを
// 返す。true の場合、そのアイテムはこのサイクルの対象から除外する
// （DESIGN.md 8章「対象の選別はオプトアウト方式とする」）。
func hasAgentLabel(labels []string) bool {
	for _, l := range labels {
		if strings.HasPrefix(l, LabelPrefix) {
			return true
		}
	}
	return false
}
