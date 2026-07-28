package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"autopilot/internal/daemon"
	"autopilot/internal/github"
	"autopilot/internal/store"
)

// Resyncer は全対象リポジトリの open な Issue/PR を走査し、DB を GitHub の現状に
// 合わせて修復する（DESIGN.md 7.5 節）。internal/daemon.Resyncer を実装する。
//
// resync はイベントを一切 enqueue しない。GitHub 上で既に close/merge された
// アイテムにエージェントを起こす意味は無く、DB に無いアイテムの登録も
// 「着火はしない」という DESIGN.md 7.6 節の方針に従うため、単なる記録に留める。
type Resyncer struct {
	Client *github.Client
	Store  *store.Store

	Repos          []string
	AllowedAuthors []string

	Logger *slog.Logger
}

var _ daemon.Resyncer = (*Resyncer)(nil)

func (r *Resyncer) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// Resync は internal/daemon.Resyncer の実装である。
func (r *Resyncer) Resync(ctx context.Context) error {
	logger := r.logger()

	reaped, err := r.Store.ReapExpiredLeases(ctx)
	if err != nil {
		return fmt.Errorf("ingest: reap expired leases: %w", err)
	}
	if reaped > 0 {
		logger.Info("resync reaped expired leases", "count", reaped)
	}

	var errs []error
	for _, repo := range r.Repos {
		if err := r.resyncRepo(ctx, repo); err != nil {
			logger.Error("resync failed for repo", "repo", repo, "error", err.Error())
			errs = append(errs, fmt.Errorf("%s: %w", repo, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Resyncer) resyncRepo(ctx context.Context, repo string) error {
	issues, err := r.Client.ListOpenIssues(ctx, repo)
	if err != nil {
		return fmt.Errorf("list open issues: %w", err)
	}
	prs, err := r.Client.ListOpenPullRequests(ctx, repo)
	if err != nil {
		return fmt.Errorf("list open pull requests: %w", err)
	}

	open := make(map[int]bool, len(issues)+len(prs))

	for _, is := range issues {
		open[is.Number] = true
		if !isAllowedAuthor(is.User.Login, r.AllowedAuthors) || hasIgnoreLabel(is.Labels) {
			continue
		}
		if _, _, err := r.Store.UpsertItem(ctx, repo, is.Number, store.KindIssue); err != nil {
			return fmt.Errorf("upsert issue #%d: %w", is.Number, err)
		}
	}

	for _, pr := range prs {
		open[pr.Number] = true
		if !isAllowedAuthor(pr.User.Login, r.AllowedAuthors) || hasIgnoreLabel(pr.Labels) {
			continue
		}
		item, _, err := r.Store.UpsertItem(ctx, repo, pr.Number, store.KindPullRequest)
		if err != nil {
			return fmt.Errorf("upsert pr #%d: %w", pr.Number, err)
		}
		if pr.HeadSHA != "" && pr.HeadSHA != item.HeadSHA {
			if err := r.Store.UpdateItemHeadSHA(ctx, item.ID, pr.HeadSHA); err != nil {
				return fmt.Errorf("update head sha for pr #%d: %w", pr.Number, err)
			}
		}
	}

	tracked, err := r.Store.ListItemsByRepo(ctx, repo)
	if err != nil {
		return fmt.Errorf("list tracked items: %w", err)
	}
	for _, it := range tracked {
		if it.Phase == store.PhaseDone {
			continue
		}
		if open[it.Number] {
			continue
		}
		if err := r.Store.UpdateItemPhase(ctx, it.ID, store.PhaseDone); err != nil {
			return fmt.Errorf("mark item #%d done: %w", it.Number, err)
		}
	}

	return nil
}
