package web

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"autopilot/internal/store"
)

// budgetDefault* は engine パッケージの既定予算（DESIGN.md 10章、engine.defaultMaxCostUSD /
// engine.defaultMaxRuns）と同じ値である。ダッシュボードは表示専用でありエンジンの実際の
// 設定値を参照できないため、進捗バーはこの既定値を前提として描画する。将来 engine 側の
// 予算が起動オプション化された場合はここも追随させる必要がある。
const (
	budgetDefaultMaxCostUSD = 5.0
	budgetDefaultMaxRuns    = 10
)

// allPhasesInOrder は DESIGN.md 6.2 節の phase 一覧を、ダッシュボードのサマリで
// 表示する順序（ワークフロー上の並び）に揃えたものである。
var allPhasesInOrder = []store.Phase{
	store.PhaseNew,
	store.PhaseAwaitingAnswer,
	store.PhaseInReview,
	store.PhaseReady,
	store.PhaseBlocked,
	store.PhaseDelegated,
	store.PhaseDone,
}

type dashboardItemRow struct {
	Item        store.Item
	LeaseHolder string // 空文字列は「lease 無し（今処理中ではない）」を表す
}

type dashboardData struct {
	Now          time.Time
	GeneratedAt  time.Time
	PhaseCounts  []phaseCount
	Items        []dashboardItemRow
	FilterPhase  string
	FilterRepo   string
	Repos        []string
	TotalItems   int
	VisibleCount int
}

type phaseCount struct {
	Phase store.Phase
	Label string
	Count int
	Href  string
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	items, err := s.store.ListAllItems(ctx)
	if err != nil {
		s.logger.Error("web: list all items", "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	now := s.now()

	counts := make(map[store.Phase]int, len(allPhasesInOrder))
	repoSet := make(map[string]struct{})
	for _, it := range items {
		counts[it.Phase]++
		repoSet[it.Repo] = struct{}{}
	}

	filterPhase := r.URL.Query().Get("phase")
	filterRepo := r.URL.Query().Get("repo")

	rows := make([]dashboardItemRow, 0, len(items))
	for _, it := range items {
		if filterPhase != "" && string(it.Phase) != filterPhase {
			continue
		}
		if filterRepo != "" && it.Repo != filterRepo {
			continue
		}

		row := dashboardItemRow{Item: it}
		if lease, ok, err := s.store.GetLease(ctx, it.ID); err != nil {
			s.logger.Error("web: get lease", "item_id", it.ID, "error", err.Error())
		} else if ok && lease.ExpiresAt.After(now) {
			row.LeaseHolder = lease.Holder
		}
		rows = append(rows, row)
	}

	repos := make([]string, 0, len(repoSet))
	for repo := range repoSet {
		repos = append(repos, repo)
	}
	sort.Strings(repos)

	phaseCounts := make([]phaseCount, 0, len(allPhasesInOrder))
	for _, p := range allPhasesInOrder {
		phaseCounts = append(phaseCounts, phaseCount{
			Phase: p,
			Label: phaseLabel(p),
			Count: counts[p],
			Href:  "/?phase=" + string(p),
		})
	}

	data := dashboardData{
		Now:          now,
		GeneratedAt:  now,
		PhaseCounts:  phaseCounts,
		Items:        rows,
		FilterPhase:  filterPhase,
		FilterRepo:   filterRepo,
		Repos:        repos,
		TotalItems:   len(items),
		VisibleCount: len(rows),
	}

	if err := dashboardTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		s.logger.Error("web: render dashboard", "error", err.Error())
	}
}

type itemData struct {
	Now             time.Time
	Item            store.Item
	Lease           *store.Lease
	Events          []store.Event
	Parent          *store.Item
	Children        []store.Item
	BudgetMaxCost   float64
	BudgetMaxRuns   int
	BudgetCostRatio float64
	BudgetRunsRatio float64
}

func (s *Server) handleItem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	owner := r.PathValue("owner")
	repoName := r.PathValue("repo")
	numberStr := r.PathValue("number")

	number, err := strconv.Atoi(numberStr)
	if err != nil {
		http.Error(w, "invalid item number", http.StatusBadRequest)
		return
	}
	repo := owner + "/" + repoName

	it, ok, err := s.store.GetItem(ctx, repo, number)
	if err != nil {
		s.logger.Error("web: get item", "repo", repo, "number", number, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}

	events, err := s.store.ListEventsByItem(ctx, it.ID)
	if err != nil {
		s.logger.Error("web: list events", "item_id", it.ID, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := itemData{
		Now:           s.now(),
		Item:          it,
		Events:        events,
		BudgetMaxCost: budgetDefaultMaxCostUSD,
		BudgetMaxRuns: budgetDefaultMaxRuns,
	}
	if budgetDefaultMaxCostUSD > 0 {
		data.BudgetCostRatio = clampRatio(it.CostUSD / budgetDefaultMaxCostUSD)
	}
	if budgetDefaultMaxRuns > 0 {
		data.BudgetRunsRatio = clampRatio(float64(it.Runs) / float64(budgetDefaultMaxRuns))
	}

	if lease, ok, err := s.store.GetLease(ctx, it.ID); err != nil {
		s.logger.Error("web: get lease", "item_id", it.ID, "error", err.Error())
	} else if ok {
		data.Lease = &lease
	}

	if it.ParentID != nil {
		if parent, ok, err := s.store.GetItemByID(ctx, *it.ParentID); err != nil {
			s.logger.Error("web: get parent item", "parent_id", *it.ParentID, "error", err.Error())
		} else if ok {
			data.Parent = &parent
		}
	}

	children, err := s.store.ListChildren(ctx, it.ID)
	if err != nil {
		s.logger.Error("web: list children", "item_id", it.ID, "error", err.Error())
	} else {
		data.Children = children
	}

	if err := itemTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		s.logger.Error("web: render item", "error", err.Error())
	}
}

func clampRatio(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
