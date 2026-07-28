package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
)

// AddLabel は repo の number（Issue/PR 番号）に対して label を付与する。
// GitHub API はラベル操作を Issue/PR で区別しないため、PR 番号を渡しても動作する。
// 既に同名のラベルが付いている場合も GitHub 側が冪等に扱うため、
// 呼び出し側で事前チェックする必要はない。
func (c *Client) AddLabel(ctx context.Context, repo string, number int, label string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/labels", repo, number)
	body := struct {
		Labels []string `json:"labels"`
	}{Labels: []string{label}}

	if err := c.request(ctx, "POST", path, body, nil); err != nil {
		return fmt.Errorf("add label %q to %s#%d: %w", label, repo, number, err)
	}
	return nil
}

// RemoveLabel は repo の number からラベル label を外す。
// 既にラベルが付いていない場合（404）はエラーとせず nil を返す
// （サイクル実行の間に人間や他の処理がラベルを変更している可能性があり、
// 「外れていればそれでよい」ため冪等に扱う）。
func (c *Client) RemoveLabel(ctx context.Context, repo string, number int, label string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/labels/%s", repo, number, url.PathEscape(label))

	err := c.request(ctx, "DELETE", path, nil, nil)
	if err == nil {
		return nil
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
		return nil
	}
	return fmt.Errorf("remove label %q from %s#%d: %w", label, repo, number, err)
}

// CreateLabel は repo に labelName のラベルを作成する。
// 既に同名のラベルが存在する場合（HTTP status 422）はエラーとせず nil を返す（冪等動作）。
func (c *Client) CreateLabel(ctx context.Context, repo string, labelName string) error {
	path := fmt.Sprintf("/repos/%s/labels", repo)
	body := struct {
		Name string `json:"name"`
	}{Name: labelName}

	err := c.request(ctx, "POST", path, body, nil)
	if err == nil {
		return nil
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == 422 {
		return nil
	}
	return fmt.Errorf("create label %q in %s: %w", labelName, repo, err)
}
