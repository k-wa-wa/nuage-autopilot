package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"autopilot/internal/daemon"
	"autopilot/internal/github"
	"autopilot/internal/store"
)

// notificationsSource は internal/store の cursors テーブルに保存する source 名である。
const notificationsSource = "notifications"

// Poller は GET /notifications を条件付きでポーリングし、変化を events として
// enqueue する（DESIGN.md 7.2〜7.4 節）。internal/daemon.Poller を実装する。
type Poller struct {
	Client *github.Client
	Store  *store.Store

	// Repos は対象リポジトリの一覧（"owner/name" 形式）である。通知は GitHub
	// アカウント全体から届くため、ここに含まれないリポジトリの通知は無視する。
	Repos []string

	// AllowedAuthors は新規アイテムの受け入れを制限する作成者一覧である。
	// 空の場合は制限しない（DESIGN.md 15章）。
	AllowedAuthors []string

	Logger *slog.Logger

	// botLogin は CurrentUser の結果のキャッシュである。Poll は単一の goroutine
	// からのみ呼ばれる（internal/daemon の worker/poller は 1 つずつしか
	// 並行実行されない）ため、同期は不要である。
	botLogin string
}

var _ daemon.Poller = (*Poller)(nil)

func (p *Poller) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// Poll は internal/daemon.Poller の実装である。
func (p *Poller) Poll(ctx context.Context) (int, error) {
	logger := p.logger()

	botLogin, err := p.currentBotLogin(ctx)
	if err != nil {
		return 0, fmt.Errorf("ingest: resolve current user: %w", err)
	}

	cursor, hadCursor, err := p.Store.GetCursor(ctx, notificationsSource)
	if err != nil {
		return 0, fmt.Errorf("ingest: get cursor: %w", err)
	}

	// prevSince は「前回このポーリングが成功した時刻」である。DB に存在しなかった
	// アイテムが新規に見つかったとき、その作成時刻がこれより後であれば真に新規の
	// Issue/PR（"opened" イベントを起こす）、そうでなければ以前から存在していた
	// ものを初めて認識しただけ（イベントを起こさず記録のみ）と判定する
	// （DESIGN.md 7.6 節）。cursor が一度も保存されていない場合（この工程の
	// 初回実行）は判定基準が無いため、常に後者として扱う。
	var prevSince time.Time
	if hadCursor && cursor.Since != "" {
		if t, err := time.Parse(time.RFC3339, cursor.Since); err == nil {
			prevSince = t
			hadCursor = true
		} else {
			hadCursor = false
		}
	} else {
		hadCursor = false
	}

	threads, lastModified, etag, notModified, err := p.Client.GetNotifications(ctx, cursor.Since, cursor.LastModified, cursor.ETag)
	if err != nil {
		return 0, fmt.Errorf("ingest: get notifications: %w", err)
	}

	total := 0
	if !notModified {
		repoSet := make(map[string]bool, len(p.Repos))
		for _, r := range p.Repos {
			repoSet[r] = true
		}

		for _, th := range threads {
			kind, ok := subjectKind(th.Subject.Type)
			if !ok {
				continue
			}
			if !repoSet[th.Repository.FullName] {
				continue
			}
			number, ok := parseSubjectNumber(th.Subject.URL)
			if !ok {
				logger.Warn("could not parse issue/pr number from notification subject url",
					"repo", th.Repository.FullName, "url", th.Subject.URL)
				continue
			}

			n, err := p.processItem(ctx, th.Repository.FullName, kind, number, botLogin, prevSince, hadCursor)
			if err != nil {
				// 1 件の失敗で他のスレッドの処理を止めない。次回のポーリングで
				// 再試行される（DB にイベントを残していないため取りこぼしはない）。
				logger.Error("failed to process notification thread",
					"repo", th.Repository.FullName, "kind", string(kind), "number", number, "error", err.Error())
				continue
			}
			total += n
		}

		newSince := time.Now().UTC().Format(time.RFC3339)
		if err := p.Store.SaveCursor(ctx, notificationsSource, etag, lastModified, newSince); err != nil {
			return total, fmt.Errorf("ingest: save cursor: %w", err)
		}
	}

	ciEnqueued, err := p.pollCheckRuns(ctx)
	if err != nil {
		logger.Error("failed to poll check runs", "error", err.Error())
	} else {
		total += ciEnqueued
	}

	return total, nil
}

// currentBotLogin は CurrentUser の結果をプロセス内でキャッシュする
// （DESIGN.md 7.3 節）。
func (p *Poller) currentBotLogin(ctx context.Context) (string, error) {
	if p.botLogin != "" {
		return p.botLogin, nil
	}
	login, err := p.Client.CurrentUser(ctx)
	if err != nil {
		return "", err
	}
	p.botLogin = login
	return login, nil
}

// processItem は 1 つの通知スレッドが指す Issue/PR を処理する。
//
// DB に無ければ「新規アイテムの候補」として扱い、対象アイテムの選別
// （NUAGE_ALLOWED_AUTHORS・agent:ignore）を行った上で登録する。真に新規の
// Issue/PR であれば "opened" イベントを起こす。既に DB にあれば、
// item.LastSeenAt 以降の新着コメント・レビューを "commented"/"reviewed"
// イベントとして enqueue する。
func (p *Poller) processItem(ctx context.Context, repo string, kind store.Kind, number int, botLogin string, prevSince time.Time, hadPrevSince bool) (int, error) {
	item, exists, err := p.Store.GetItem(ctx, repo, number)
	if err != nil {
		return 0, fmt.Errorf("get item: %w", err)
	}

	if !exists {
		return p.admitAndBaseline(ctx, repo, kind, number, prevSince, hadPrevSince)
	}

	return p.diffComments(ctx, item, kind, botLogin)
}

// admitAndBaseline は DB に無い Issue/PR を新規登録する。
func (p *Poller) admitAndBaseline(ctx context.Context, repo string, kind store.Kind, number int, prevSince time.Time, hadPrevSince bool) (int, error) {
	detail, err := fetchDetail(ctx, p.Client, repo, kind, number)
	if err != nil {
		return 0, fmt.Errorf("fetch detail: %w", err)
	}

	if !isAllowedAuthor(detail.Author, p.AllowedAuthors) || hasIgnoreLabel(detail.Labels) {
		// 対象外。DB に行を作らない（次回以降の通知でも同じ判定を繰り返すことに
		// なるが、対象外アイテムの活動頻度は通常低く、許容できるコストである）。
		return 0, nil
	}

	item, _, err := p.Store.UpsertItem(ctx, repo, number, kind)
	if err != nil {
		return 0, fmt.Errorf("upsert item: %w", err)
	}

	if detail.HeadSHA != "" {
		if err := p.Store.UpdateItemHeadSHA(ctx, item.ID, detail.HeadSHA); err != nil {
			return 0, fmt.Errorf("update head sha: %w", err)
		}
	}

	// last_seen_at を Issue/PR 自身の作成時刻でベースラインする。本文以外に
	// まだコメントを 1 件も確認していないため、次回以降はこの時刻より新しい
	// コメントだけを新着として扱えばよい。
	if err := p.Store.UpdateItemLastSeenAt(ctx, item.ID, detail.CreatedAt); err != nil {
		return 0, fmt.Errorf("baseline last_seen_at: %w", err)
	}

	if !hadPrevSince || !detail.CreatedAt.After(prevSince) {
		// 以前から存在していたアイテムを初めて認識しただけ。着火しない
		// （DESIGN.md 7.6 節）。
		return 0, nil
	}

	dedupKey := fmt.Sprintf("opened:%s#%d", repo, number)
	_, inserted, err := p.Store.EnqueueEvent(ctx, dedupKey, item.ID, "opened", detail.Author, detail.Body, detail.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("enqueue opened event: %w", err)
	}
	if inserted {
		return 1, nil
	}
	return 0, nil
}

// diffComments は既存アイテムについて、item.LastSeenAt 以降の新着コメント・
// レビューを events に enqueue する。PR の場合、push はコメントを伴わないため
// 別途 head_sha を再取得して最新化する。
func (p *Poller) diffComments(ctx context.Context, item store.Item, kind store.Kind, botLogin string) (int, error) {
	if item.LastSeenAt == nil {
		// last_seen_at が未確立（resync がこのアイテムを先に登録しただけで、
		// まだ poller が一度も処理していない）。基準点が無いまま差分を取ると
		// 全既存コメントが「新着」になってしまうため、今の時点で静かに
		// ベースラインするに留める。
		if err := p.Store.UpdateItemLastSeenAt(ctx, item.ID, time.Now()); err != nil {
			return 0, fmt.Errorf("baseline last_seen_at: %w", err)
		}
		return 0, nil
	}

	comments, err := p.Client.ListCommentsSince(ctx, item.Repo, item.Number, *item.LastSeenAt, kind == store.KindPullRequest)
	if err != nil {
		return 0, fmt.Errorf("list comments since: %w", err)
	}

	total := 0
	latest := *item.LastSeenAt
	for _, c := range comments {
		if c.CreatedAt.After(latest) {
			latest = c.CreatedAt
		}
		if isSelfOrBot(c, botLogin) {
			continue
		}

		eventType := "commented"
		if c.Kind == github.CommentKindReview {
			eventType = "reviewed"
		}
		dedupKey := fmt.Sprintf("%s:%d", eventType, c.ID)
		_, inserted, err := p.Store.EnqueueEvent(ctx, dedupKey, item.ID, eventType, c.User.Login, c.Body, c.CreatedAt)
		if err != nil {
			return total, fmt.Errorf("enqueue %s event: %w", eventType, err)
		}
		if inserted {
			total++
		}
	}

	if kind == store.KindPullRequest {
		if pr, err := p.Client.GetPullRequest(ctx, item.Repo, item.Number); err != nil {
			p.logger().Warn("failed to refresh pr head sha", "repo", item.Repo, "number", item.Number, "error", err.Error())
		} else if pr.HeadSHA != "" && pr.HeadSHA != item.HeadSHA {
			if err := p.Store.UpdateItemHeadSHA(ctx, item.ID, pr.HeadSHA); err != nil {
				return total, fmt.Errorf("update head sha: %w", err)
			}
		}
	}

	if latest.After(*item.LastSeenAt) {
		if err := p.Store.UpdateItemLastSeenAt(ctx, item.ID, latest); err != nil {
			return total, fmt.Errorf("advance last_seen_at: %w", err)
		}
	}

	return total, nil
}

// pollCheckRuns は phase=in_review のアイテムについて CI チェックランの状態を
// 取得し、成功・失敗が確定していれば ci_success/ci_failure イベントを enqueue
// する（DESIGN.md 7.4 節）。
//
// dedup_key に head_sha と状態を含めることで、「同じコミットの同じ結果」を
// 二重に enqueue することを防ぐ。新しいコミットが積まれれば head_sha が変わり、
// 新しい判定が起こせる。専用のカラムで前回状態を保持する必要が無い。
func (p *Poller) pollCheckRuns(ctx context.Context) (int, error) {
	items, err := p.Store.ListItemsByPhase(ctx, store.PhaseInReview)
	if err != nil {
		return 0, fmt.Errorf("list in_review items: %w", err)
	}

	total := 0
	for _, it := range items {
		if it.HeadSHA == "" {
			continue
		}
		state, err := p.Client.GetCheckState(ctx, it.Repo, it.HeadSHA)
		if err != nil {
			p.logger().Warn("failed to fetch check state", "repo", it.Repo, "number", it.Number, "error", err.Error())
			continue
		}

		var eventType string
		switch state {
		case "success":
			eventType = "ci_success"
		case "failure":
			eventType = "ci_failure"
		default:
			continue
		}

		dedupKey := fmt.Sprintf("checkrun:%s#%d:%s:%s", it.Repo, it.Number, it.HeadSHA, state)
		_, inserted, err := p.Store.EnqueueEvent(ctx, dedupKey, it.ID, eventType, "github", "", time.Now())
		if err != nil {
			return total, fmt.Errorf("enqueue %s event: %w", eventType, err)
		}
		if inserted {
			total++
		}
	}
	return total, nil
}

// itemDetail は新規アイテムの選別・登録に必要な情報をまとめたものである。
// Issue と PullRequest のどちらから来たかによらず同じ形で扱えるようにする。
type itemDetail struct {
	Author    string
	Body      string
	Labels    []string
	HeadSHA   string
	CreatedAt time.Time
}

func fetchDetail(ctx context.Context, client *github.Client, repo string, kind store.Kind, number int) (itemDetail, error) {
	if kind == store.KindPullRequest {
		pr, err := client.GetPullRequest(ctx, repo, number)
		if err != nil {
			return itemDetail{}, err
		}
		return itemDetail{
			Author:    pr.User.Login,
			Body:      pr.Body,
			Labels:    pr.Labels,
			HeadSHA:   pr.HeadSHA,
			CreatedAt: pr.CreatedAt,
		}, nil
	}

	issue, err := client.GetIssue(ctx, repo, number)
	if err != nil {
		return itemDetail{}, err
	}
	return itemDetail{
		Author:    issue.User.Login,
		Body:      issue.Body,
		Labels:    issue.Labels,
		CreatedAt: issue.CreatedAt,
	}, nil
}
