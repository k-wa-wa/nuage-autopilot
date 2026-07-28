package github

import (
	"context"
	"fmt"
)

// ListOpenPullRequests は repo（"owner/name" 形式）の open な PR 一覧を取得する。
// /pulls エンドポイントは PR のみを返すため、Issue との混同は起きない。
func (c *Client) ListOpenPullRequests(ctx context.Context, repo string) ([]PullRequest, error) {
	path := fmt.Sprintf("/repos/%s/pulls?state=open&per_page=%d", repo, listPerPage)

	var raw []rawPullRequest
	if err := c.request(ctx, "GET", path, nil, &raw); err != nil {
		return nil, fmt.Errorf("list open pull requests for %s: %w", repo, err)
	}

	prs := make([]PullRequest, 0, len(raw))
	for _, r := range raw {
		prs = append(prs, r.toPullRequest())
	}
	return prs, nil
}

// GetPullRequest は repo の number 番の PR 単体を取得する。
//
// worker 実行後、push によって変化した可能性のある head SHA を権威ある情報源から
// 再取得するために使う（internal/cycle/executor.go が状態行の sha= フィールドを
// 埋めるために呼ぶ）。EnsureWorkspace で clone したローカルの HEAD ではなく
// GitHub 側の状態を正とする。
func (c *Client) GetPullRequest(ctx context.Context, repo string, number int) (PullRequest, error) {
	path := fmt.Sprintf("/repos/%s/pulls/%d", repo, number)

	var raw rawPullRequest
	if err := c.request(ctx, "GET", path, nil, &raw); err != nil {
		return PullRequest{}, fmt.Errorf("get pull request %s#%d: %w", repo, number, err)
	}
	return raw.toPullRequest(), nil
}
