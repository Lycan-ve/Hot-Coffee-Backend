package webqueue

import (
	"testing"
	"time"

	"smash-backend/internal/models"
)

func TestQueueSearchProgressForGlickoPhases(t *testing.T) {
	now := time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC)
	self := models.MatchmakingQueue{
		DiscordID: "self",
		Points:    0,
		JoinedAt:  now.Add(-10 * time.Second),
	}

	tests := []struct {
		name          string
		entries       []models.MatchmakingQueue
		playerRatings map[string]models.MatchmakingPlayer
		wantPhase     string
		wantCount     int
		wantText      string
	}{
		{
			name:      "no rivals",
			entries:   []models.MatchmakingQueue{self},
			wantPhase: searchPhaseAnalyzing,
			wantText:  "ANALIZANDO TU RATING…",
		},
		{
			name: "player outside current Glicko window",
			entries: []models.MatchmakingQueue{
				self,
				{DiscordID: "far-rating", JoinedAt: now.Add(-5 * time.Second)},
			},
			playerRatings: map[string]models.MatchmakingPlayer{
				"self":      {DiscordID: "self", GlickoRating: 1500, GlickoRD: 40, GlickoVolatility: 0.06},
				"far-rating": {DiscordID: "far-rating", GlickoRating: 2300, GlickoRD: 40, GlickoVolatility: 0.06},
			},
			wantPhase: searchPhaseMeasuring,
			wantText:  "MIDIENDO EL NIVEL DE LA COLA…",
		},
		{
			name: "similar Glicko rating and overlapping uncertainty",
			entries: []models.MatchmakingQueue{
				self,
				{DiscordID: "near-rating", JoinedAt: now.Add(-5 * time.Second)},
			},
			playerRatings: map[string]models.MatchmakingPlayer{
				"self":        {DiscordID: "self", GlickoRating: 1500, GlickoRD: 100, GlickoVolatility: 0.06},
				"near-rating": {DiscordID: "near-rating", GlickoRating: 1600, GlickoRD: 100, GlickoVolatility: 0.06},
			},
			wantPhase: searchPhaseContacting,
			wantCount: 1,
			wantText:  "CONTACTANDO RIVALES CERCANOS…",
		},
		{
			name: "Glicko window expands after waiting",
			entries: []models.MatchmakingQueue{
				{DiscordID: "self", JoinedAt: now.Add(-3 * time.Minute)},
				{DiscordID: "wider-rating", JoinedAt: now.Add(-3 * time.Minute)},
			},
			playerRatings: map[string]models.MatchmakingPlayer{
				"self":         {DiscordID: "self", GlickoRating: 1500, GlickoRD: 100, GlickoVolatility: 0.06},
				"wider-rating": {DiscordID: "wider-rating", GlickoRating: 1770, GlickoRD: 100, GlickoVolatility: 0.06},
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
			got := queueSearchProgressFor(entry, tt.entries, tt.playerRatings, nil, now)
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

func TestCanPairByGlickoUsesCombinedRDAndWaitExpansion(t *testing.T) {
	a := models.MatchmakingQueue{DiscordID: "a"}
	b := models.MatchmakingQueue{DiscordID: "b"}
	ratings := map[string]models.MatchmakingPlayer{
		"a": {DiscordID: "a", GlickoRating: 1500, GlickoRD: 100, GlickoVolatility: 0.06},
		"b": {DiscordID: "b", GlickoRating: 1770, GlickoRD: 100, GlickoVolatility: 0.06},
	}

	if canPairByGlicko(a, b, ratings, 10*time.Second) {
		t.Fatal("players should not pair inside the narrow initial window")
	}
	if !canPairByGlicko(a, b, ratings, 3*time.Minute) {
		t.Fatal("players should pair after the Glicko window expands")
	}
}

func TestQueueSearchProgressDoesNotAdvertiseBlockedRematch(t *testing.T) {
	now := time.Now().UTC()
	entry := models.MatchmakingQueue{DiscordID: "self", JoinedAt: now.Add(-10 * time.Second)}
	peer := models.MatchmakingQueue{DiscordID: "peer", JoinedAt: now.Add(-5 * time.Second)}
	rematches := map[string]bool{"peer|self": true}
	ratings := map[string]models.MatchmakingPlayer{
		"self": {DiscordID: "self", GlickoRating: 1500, GlickoRD: 100, GlickoVolatility: 0.06},
		"peer": {DiscordID: "peer", GlickoRating: 1500, GlickoRD: 100, GlickoVolatility: 0.06},
	}

	got := queueSearchProgressFor(entry, []models.MatchmakingQueue{entry, peer}, ratings, rematches, now)
	if got.Phase != searchPhaseMeasuring {
		t.Fatalf("phase = %q, want %q", got.Phase, searchPhaseMeasuring)
	}
	if got.CandidateCount != 0 {
		t.Fatalf("candidate count = %d, want 0", got.CandidateCount)
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
