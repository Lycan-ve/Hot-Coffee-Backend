package ranks

import "testing"

func TestFromPointsStartsAtZeroRP(t *testing.T) {
	got := FromPoints(0)
	if got.RP != 0 || got.Points != 0 {
		t.Fatalf("zero RP start = RP %d / points %d, want 0 / 0", got.RP, got.Points)
	}
	if got.DisplayName != "Aspirant" {
		t.Fatalf("zero RP rank = %q, want Aspirant", got.DisplayName)
	}
	if got.RPToNext != 100 {
		t.Fatalf("zero RP to next = %d, want 100", got.RPToNext)
	}
}

func TestRPRankThresholds(t *testing.T) {
	tests := []struct {
		rp       int
		wantRank string
		wantTier string
	}{
		{rp: 0, wantRank: "Aspirant"},
		{rp: 99, wantRank: "Aspirant"},
		{rp: 100, wantRank: "Iron", wantTier: "I"},
		{rp: 200, wantRank: "Iron", wantTier: "II"},
		{rp: 300, wantRank: "Iron", wantTier: "III"},
		{rp: 1000, wantRank: "Gold", wantTier: "I"},
		{rp: 2500, wantRank: "Grandmaster"},
		{rp: 2600, wantRank: "Challenger"},
	}

	for _, tt := range tests {
		t.Run(tt.wantRank+"_"+tt.wantTier, func(t *testing.T) {
			got := FromPoints(tt.rp)
			if got.Rank != tt.wantRank || got.Tier != tt.wantTier {
				t.Fatalf("FromPoints(%d) = %s %s, want %s %s", tt.rp, got.Rank, got.Tier, tt.wantRank, tt.wantTier)
			}
			if got.RP != tt.rp || got.Points != tt.rp {
				t.Fatalf("FromPoints(%d) returned RP=%d points=%d", tt.rp, got.RP, got.Points)
			}
		})
	}
}

func TestNegativeRPIsClampedToZero(t *testing.T) {
	got := FromPoints(-250)
	if got.RP != 0 || got.Points != 0 || got.Rank != "Aspirant" {
		t.Fatalf("negative RP should clamp to Aspirant at 0 RP, got %+v", got)
	}
}

func TestDivisionHelpersUseZeroBasedRP(t *testing.T) {
	tests := []struct {
		rp   int
		want int
	}{
		{rp: 0, want: 0},
		{rp: 99, want: 0},
		{rp: 100, want: 1},
		{rp: 1000, want: 10},
		{rp: 2500, want: GMIndex},
		{rp: 2600, want: ChallengerIndex},
	}
	for _, tt := range tests {
		if got := DivisionIndexOf(tt.rp); got != tt.want {
			t.Errorf("DivisionIndexOf(%d) = %d, want %d", tt.rp, got, tt.want)
		}
	}

	if got := DivisionFloor(0); got != 0 {
		t.Errorf("Aspirant floor = %d, want 0", got)
	}
	if got := DivisionFloor(1); got != 100 {
		t.Errorf("Iron I floor = %d, want 100", got)
	}
	if got := DivisionFloor(GMIndex); got != 2500 {
		t.Errorf("Grandmaster floor = %d, want 2500", got)
	}
}

func TestRankProgressIsAlwaysRP(t *testing.T) {
	got := FromPoints(145)
	if got.RP != 145 || got.RPToNext != 55 {
		t.Fatalf("145 RP state = RP %d, RPToNext %d; want 145 and 55", got.RP, got.RPToNext)
	}
	if got.Progress != 45 {
		t.Fatalf("145 RP progress = %.2f, want 45", got.Progress)
	}
}

func TestPlacementAndRankedFloorsAreZeroRP(t *testing.T) {
	if PlacementFloor != 0 {
		t.Fatalf("PlacementFloor = %d, want 0", PlacementFloor)
	}
	if MinRPFloor != 0 {
		t.Fatalf("MinRPFloor = %d, want 0", MinRPFloor)
	}
}
