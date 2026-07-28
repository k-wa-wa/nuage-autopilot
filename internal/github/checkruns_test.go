package github

import (
	"context"
	"net/http"
	"testing"
)

func TestGetCheckState(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantState  string
		wantErr    bool
	}{
		{
			name:       "empty check runs",
			statusCode: http.StatusOK,
			body:       `{"total_count": 0, "check_runs": []}`,
			wantState:  "none",
			wantErr:    false,
		},
		{
			name:       "all success",
			statusCode: http.StatusOK,
			body:       `{"total_count": 2, "check_runs": [{"status": "completed", "conclusion": "success"}, {"status": "completed", "conclusion": "neutral"}]}`,
			wantState:  "success",
			wantErr:    false,
		},
		{
			name:       "has pending",
			statusCode: http.StatusOK,
			body:       `{"total_count": 2, "check_runs": [{"status": "completed", "conclusion": "success"}, {"status": "in_progress", "conclusion": null}]}`,
			wantState:  "pending",
			wantErr:    false,
		},
		{
			name:       "has failure",
			statusCode: http.StatusOK,
			body:       `{"total_count": 2, "check_runs": [{"status": "completed", "conclusion": "failure"}, {"status": "in_progress", "conclusion": null}]}`,
			wantState:  "failure",
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/owner/repo/commits/sha123/check-runs" {
					t.Fatalf("unexpected path: %s", r.URL.Path)
				}
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			})

			got, err := client.GetCheckState(context.Background(), "owner/repo", "sha123")
			if (err != nil) != tt.wantErr {
				t.Fatalf("GetCheckState() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.wantState {
				t.Errorf("GetCheckState() = %s, want %s", got, tt.wantState)
			}
		})
	}
}
