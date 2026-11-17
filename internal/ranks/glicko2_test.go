package ranks

import (
	"math"
	"testing"
	"time"
)

func TestGlicko2OfficialExample(t *testing.T) {
	// Ejemplo de referencia de Mark E. Glickman:
	// 1500/RD200/σ0.06 contra tres rivales (una victoria y dos derrotas).
	now := time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC)
	got := UpdateGlicko2(
		Glicko2Rating{Rating: 1500, RD: 200, Volatility: 0.06},
		[]Glicko2Opponent{
			{Rating: 1400, RD: 30, Score: 1},
			{Rating: 1550, RD: 100, Score: 0},
			{Rating: 1700, RD: 300, Score: 0},
		},
		now,
	)

	assertNear(t, "rating", got.Rating, 1464.06, 0.02)
	assertNear(t, "RD", got.RD, 151.52, 0.02)
	assertNear(t, "volatility", got.Volatility, 0.059996, 0.000002)
	if got.LastRatedAt == nil || !got.LastRatedAt.Equal(now) {
		t.Fatalf("LastRatedAt = %v, want %v", got.LastRatedAt, now)
	}
}

func TestGlicko2PairMovesRatingsInOppositeDirections(t *testing.T) {
	now := time.Now().UTC()
	initial := DefaultGlicko2Rating()
	winner, loser := UpdateGlicko2Pair(initial, initial, 1, now)

	if winner.Rating <= initial.Rating {
		t.Fatalf("winner rating = %.3f, want greater than %.3f", winner.Rating, initial.Rating)
	}
	if loser.Rating >= initial.Rating {
		t.Fatalf("loser rating = %.3f, want less than %.3f", loser.Rating, initial.Rating)
	}
	if winner.RD >= initial.RD || loser.RD >= initial.RD {
		t.Fatalf("RD should fall after a rated set: winner %.3f, loser %.3f", winner.RD, loser.RD)
	}
}

func TestGlicko2ExpectedScoreIsSymmetric(t *testing.T) {
	a := Glicko2Rating{Rating: 1700, RD: 100, Volatility: 0.06}
	b := Glicko2Rating{Rating: 1400, RD: 100, Volatility: 0.06}
	ab := Glicko2ExpectedScore(a, b)
	ba := Glicko2ExpectedScore(b, a)
	assertNear(t, "complementary expected score", ab+ba, 1, 1e-12)
	if ab <= 0.5 || ba >= 0.5 {
		t.Fatalf("expected A to be favoured: A=%.4f B=%.4f", ab, ba)
	}
}

func TestGlicko2DistanceUsesBothUncertainties(t *testing.T) {
	a := Glicko2Rating{Rating: 1600, RD: 100, Volatility: 0.06}
	b := Glicko2Rating{Rating: 1500, RD: 100, Volatility: 0.06}
	got := Glicko2DistanceSigma(a, b)
	want := 100 / math.Hypot(100, 100)
	assertNear(t, "normalized distance", got, want, 1e-12)
}

func TestGlicko2RDInflatesAfterInactivity(t *testing.T) {
	lastRated := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	rating := Glicko2Rating{
		Rating: 1600, RD: 100, Volatility: 0.06, LastRatedAt: &lastRated,
	}
	now := lastRated.Add(3 * Glicko2InactivityPeriod)
	got := Glicko2At(rating, now)
	if got.RD <= rating.RD {
		t.Fatalf("RD after inactivity = %.3f, want > %.3f", got.RD, rating.RD)
	}
	if got.RD > Glicko2DefaultRD {
		t.Fatalf("RD after inactivity = %.3f, should be capped at %.3f", got.RD, Glicko2DefaultRD)
	}
	if got.Rating != rating.Rating {
		t.Fatalf("rating changed during inactivity: got %.3f, want %.3f", got.Rating, rating.Rating)
	}
}

func TestGlicko2DefaultsForUninitializedPlayers(t *testing.T) {
	got := normalizeGlicko2(Glicko2Rating{})
	want := DefaultGlicko2Rating()
	if got != want {
		t.Fatalf("normalized rating = %+v, want %+v", got, want)
	}
}

func assertNear(t *testing.T, label string, got, want, tolerance float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Fatalf("%s = %.8f, want %.8f ± %.8f", label, got, want, tolerance)
	}
}
