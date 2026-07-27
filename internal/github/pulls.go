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
