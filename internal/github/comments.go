package github

import (
	"context"
	"fmt"
)

// ListComments は repo の number（Issue/PR 番号）に付いた会話コメントを一覧取得する。
// Issue と PR は同一のコメントエンドポイントを共有するため、この関数はどちらの
// 番号に対しても使える。
func (c *Client) ListComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=%d", repo, number, listPerPage)

	var raw []rawComment
	if err := c.request(ctx, "GET", path, nil, &raw); err != nil {
		return nil, fmt.Errorf("list comments for %s#%d: %w", repo, number, err)
	}

	comments := make([]Comment, 0, len(raw))
	for _, r := range raw {
		comments = append(comments, r.toComment())
	}
	return comments, nil
}

// CreateComment は repo の number に対して本文 body のコメントを投稿する。
func (c *Client) CreateComment(ctx context.Context, repo string, number int, body string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number)
	reqBody := struct {
		Body string `json:"body"`
	}{Body: body}

	if err := c.request(ctx, "POST", path, reqBody, nil); err != nil {
		return fmt.Errorf("create comment on %s#%d: %w", repo, number, err)
	}
	return nil
}
