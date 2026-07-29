package engine

import (
	"testing"

	"autopilot/internal/store"
)

func TestNextAction(t *testing.T) {
	tests := []struct {
		name  string
		phase store.Phase
		event string
		want  action
	}{
		{"new+opened launches new session", store.PhaseNew, "opened", actionLaunchNew},
		{"new+commented launches new session", store.PhaseNew, "commented", actionLaunchNew},
		{"new+ci_success is ignored", store.PhaseNew, "ci_success", actionIgnore},

		{"awaiting_answer+commented resumes", store.PhaseAwaitingAnswer, "commented", actionLaunchResume},
		{"awaiting_answer+reviewed is ignored (not a valid trigger for this phase)", store.PhaseAwaitingAnswer, "reviewed", actionIgnore},

		{"blocked+commented resumes", store.PhaseBlocked, "commented", actionLaunchResume},

		{"in_review+ci_failure resumes", store.PhaseInReview, "ci_failure", actionLaunchResume},
		{"in_review+ci_success moves to ready", store.PhaseInReview, "ci_success", actionToReady},
		{"in_review+commented resumes", store.PhaseInReview, "commented", actionLaunchResume},
		{"in_review+reviewed resumes", store.PhaseInReview, "reviewed", actionLaunchResume},

		{"ready+commented resumes", store.PhaseReady, "commented", actionLaunchResume},
		{"ready+reviewed resumes", store.PhaseReady, "reviewed", actionLaunchResume},
		{"ready+ci_failure returns to in_review and launches", store.PhaseReady, "ci_failure", actionToInReviewAndLaunch},
		{"ready+ci_success is ignored", store.PhaseReady, "ci_success", actionIgnore},

		{"delegated+child_done resumes", store.PhaseDelegated, "child_done", actionLaunchResume},
		{"delegated+commented is ignored (parent does not act directly while delegated)", store.PhaseDelegated, "commented", actionIgnore},

		{"done+anything is ignored", store.PhaseDone, "commented", actionIgnore},

		{"closed always goes to done regardless of phase", store.PhaseInReview, "closed", actionToDone},
		{"merged always goes to done regardless of phase", store.PhaseReady, "merged", actionToDone},
		{"closed on an already-done item is still to_done (idempotent)", store.PhaseDone, "closed", actionToDone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextAction(tt.phase, tt.event); got != tt.want {
				t.Fatalf("nextAction(%q, %q) = %q, want %q", tt.phase, tt.event, got, tt.want)
			}
		})
	}
}
