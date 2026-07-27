package cycle

import (
	"context"
	"testing"
	"time"
)

func TestRun_ReturnsInputParameters(t *testing.T) {
	before := time.Now()

	result, err := Run(context.Background(), "k-wa-wa/pechka", "/var/lib/nuage-autopilot")
	if err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}

	after := time.Now()

	if result.Repo != "k-wa-wa/pechka" {
		t.Fatalf("result.Repo = %q, want %q", result.Repo, "k-wa-wa/pechka")
	}
	if result.StateDir != "/var/lib/nuage-autopilot" {
		t.Fatalf("result.StateDir = %q, want %q", result.StateDir, "/var/lib/nuage-autopilot")
	}
	if result.StartedAt.Before(before) || result.StartedAt.After(after) {
		t.Fatalf("result.StartedAt = %v, want between %v and %v", result.StartedAt, before, after)
	}
}
