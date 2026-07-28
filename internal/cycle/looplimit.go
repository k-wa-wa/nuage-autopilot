package cycle

import (
	"sort"

	"github.com/k-wa-wa/nuage-workspace/autopilot/internal/github"
)

// LoopLimit は DESIGN.md 8章「ループ上限（Go 側の硬い制限）」に定める既定の上限値である。
// 最後の人間のコメント以降に投稿された Bot コメントの数がこれ以上になったアイテムは、
// dispatcher の判断を経ずに LabelAwaitingUserReview を付与して処理を止める。
const LoopLimit = 5

// botCommentsSinceLastHuman は comments を新しい順に見て、最後の人間のコメント以降に
// 投稿された Bot コメントの数を数える。
//
// 人間のコメントが 1 件も無い場合はすべての Bot コメントを数える
// （人間の関与が唯一の脱出口であるというモデルと一致させるため、この場合も
// 上限に達すれば止めるべきである）。
//
// Bot 判定は botLogin（internal/github.Client.CurrentUser の戻り値）とコメント投稿者の
// Login が一致するか、投稿者の Type が "Bot" であるかのいずれかで行う
// （Phase 2 の shouldClearWait と同じ考え方。GitHub App 経由でなく専用の
// Personal Access Token アカウントを使う運用では Type は "User" のままになるため、
// Type だけでは判定できない）。
func botCommentsSinceLastHuman(comments []github.Comment, botLogin string) int {
	sorted := make([]github.Comment, len(comments))
	copy(sorted, comments)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
	})

	count := 0
	for _, c := range sorted {
		if isBotComment(c, botLogin) {
			count++
			continue
		}
		break
	}
	return count
}

// isBotComment は c が autopilot 自身（または他の Bot）による投稿かどうかを判定する。
func isBotComment(c github.Comment, botLogin string) bool {
	return c.User.Login == botLogin || c.User.Type == "Bot"
}
