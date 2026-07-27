package github

import (
	"context"
	"fmt"
)

// listPerPage は 1 ページあたりの取得件数である。
//
// TODO: 現状は先頭 1 ページ（最大 100 件）のみを取得する。対象リポジトリの
// open な Issue/PR がこれを超える運用になった場合はページネーション
// （Link ヘッダの追跡）の実装が必要になる。Phase 2 の時点では対象リポジトリ
// （pechka 等）の open 件数がそこまで多くない前提で単純化した。
const listPerPage = 100

// ListOpenIssues は repo（"owner/name" 形式）の open な Issue 一覧を取得する。
// GitHub の Issue API は PR も同じレスポンスに含めて返すため、
// pull_request フィールドを持つ要素は明示的に除外し、Issue のみを返す
// （PR は ListOpenPullRequests で別途取得する）。
func (c *Client) ListOpenIssues(ctx context.Context, repo string) ([]Issue, error) {
	path := fmt.Sprintf("/repos/%s/issues?state=open&per_page=%d", repo, listPerPage)

	var raw []rawIssue
	if err := c.request(ctx, "GET", path, nil, &raw); err != nil {
		return nil, fmt.Errorf("list open issues for %s: %w", repo, err)
	}

	issues := make([]Issue, 0, len(raw))
	for _, r := range raw {
		if r.isPullRequest() {
			continue
		}
		issues = append(issues, r.toIssue())
	}
	return issues, nil
}
