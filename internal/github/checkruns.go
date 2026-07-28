package github

import (
	"context"
	"fmt"
	"net/http"
)

// GetCheckState は対象コミット（sha または ref）の Check Runs 状態を集約して返す。
// 返り値: "success", "failure", "pending", "none" (チェックランなし)
func (c *Client) GetCheckState(ctx context.Context, repo, ref string) (string, error) {
	if ref == "" {
		return "none", nil
	}

	path := fmt.Sprintf("/repos/%s/commits/%s/check-runs", repo, ref)
	var resp CheckRunsResponse
	if err := c.request(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return "", err
	}

	if resp.TotalCount == 0 {
		return "none", nil
	}

	hasPending := false
	hasFailure := false
	for _, cr := range resp.CheckRuns {
		if cr.Status != "completed" {
			hasPending = true
		} else if cr.Conclusion != "success" && cr.Conclusion != "neutral" && cr.Conclusion != "skipped" {
			hasFailure = true
		}
	}

	if hasFailure {
		return "failure", nil
	}
	if hasPending {
		return "pending", nil
	}
	return "success", nil
}
