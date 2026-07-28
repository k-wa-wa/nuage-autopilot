package github

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// ListComments は repo の number（Issue/PR 番号）に付いたコメントを一覧取得する。
// Issue の会話コメントに加えて、PR レビューコメント（GET /pulls/{n}/reviews）も
// 取得・統合し、時系列順にソートして返す。
func (c *Client) ListComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=%d&sort=created&direction=desc", repo, number, listPerPage)

	var raw []rawComment
	if err := c.request(ctx, "GET", path, nil, &raw); err != nil {
		return nil, fmt.Errorf("list comments for %s#%d: %w", repo, number, err)
	}

	comments := make([]Comment, 0, len(raw))
	for _, r := range raw {
		comments = append(comments, r.toComment())
	}

	// PR レビューコメントも取得を試みる（404 の場合は対象が Issue のため無視する）。
	reviewsPath := fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=%d", repo, number, listPerPage)
	var rawReviews []rawReview
	if err := c.request(ctx, "GET", reviewsPath, nil, &rawReviews); err == nil {
		for _, r := range rawReviews {
			if r.Body != "" {
				comments = append(comments, r.toComment())
			}
		}
	} else {
		var apiErr *APIError
		if !(errors.As(err, &apiErr) && apiErr.StatusCode == 404) {
			// 404 以外のエラーでも会話コメントの返却を優先し無視する。
		}
	}

	sort.Slice(comments, func(i, j int) bool {
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})

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
