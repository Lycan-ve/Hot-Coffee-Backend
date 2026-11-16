package webqueue

import (
	"testing"
	"time"

	"smash-backend/internal/models"
)

func TestQueueSearchProgressForPhases(t *testing.T) {
	now := time.Date(2026, time.November, 16, 12, 0, 0, 0, time.UTC)
	self := models.MatchmakingQueue{
		DiscordID: "self",
		Points:    0,
		JoinedAt:  now.Add(-10 * time.Second),
	}

	tests := []struct {
		name       string
		entries    []models.MatchmakingQueue
		streaks    map[string]int
		wantPhase  string
		wantCount  int
		wantText   string
	}{
		{
			name:      "no rivals",
			entries:   []models.MatchmakingQueue{self},
			wantPhase: searchPhaseAnalyzing,
			wantText:  "ANALIZANDO TU RANGO…",
		},
		{
			name: "players in queue but ranks are not compatible yet",
			entries: []models.MatchmakingQueue{
				self,
				{DiscordID: "far-rank", Points: 600, JoinedAt: now.Add(-5 * time.Second)},
			},
			wantPhase: searchPhaseMeasuring,
			wantText:  "MIDIENDO EL NIVEL DE LA COLA…",
		},
		{
			name: "compatible rival",
			entries: []models.MatchmakingQueue{
				self,
				{DiscordID: "same-rank", Points: 0, JoinedAt: now.Add(-5 * time.Second)},
			},
			wantPhase: searchPhaseContacting,
			wantCount: 1,
			wantText:  "CONTACTANDO RIVALES CERCANOS…",
		},
		{
			name: "adjacent rank becomes eligible after waiting threshold",
			entries: []models.MatchmakingQueue{
				{DiscordID: "self", Points: 0, JoinedAt: now.Add(-3 * time.Minute)},
				{DiscordID: "adjacent-rank", Points: 100, JoinedAt: now.Add(-3 * time.Minute)},
			},
			wantPhase: searchPhaseContacting,
			wantCount: 1,
			wantText:  "CONTACTANDO RIVALES CERCANOS…",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := self
			for i := range tt.entries {
				if tt.entries[i].DiscordID == "self" {
					entry = tt.entries[i]
					break
				}
			}
			got := queueSearchProgressFor(entry, tt.entries, tt.streaks, now)
			if got.Phase != tt.wantPhase {
				t.Fatalf("phase = %q, want %q", got.Phase, tt.wantPhase)
			}
			if got.CandidateCount != tt.wantCount {
				t.Fatalf("candidate count = %d, want %d", got.CandidateCount, tt.wantCount)
			}
			if got.Message != tt.wantText {
				t.Fatalf("message = %q, want %q", got.Message, tt.wantText)
			}
		})
	}
}

func TestQueueSearchProgressPreparingMessage(t *testing.T) {
	now := time.Now().UTC()
	got := newQueueSearchProgress(searchPhasePreparing, 0, 2*time.Second, now)
	if got.Message != "PREPARANDO EL ESCENARIO…" {
		t.Fatalf("message = %q", got.Message)
	}
	if got.ElapsedSeconds != 2 {
		t.Fatalf("elapsed seconds = %d, want 2", got.ElapsedSeconds)
	}
	if !got.UpdatedAt.Equal(now) {
		t.Fatalf("updatedAt = %v, want %v", got.UpdatedAt, now)
	}
}
