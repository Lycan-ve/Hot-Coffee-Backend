package ranks

import (
	"math"
	"time"
)

// Glicko-2 usa la escala original de Glickman (1500, RD 350).
// Cada set confirmado constituye un periodo online. Para inactividad, la
// desviación se amplía por periodos semanales sin resultados confirmados.
const (
	Glicko2DefaultRating     = 1500.0
	Glicko2DefaultRD         = 350.0
	Glicko2DefaultVolatility = 0.06

	glicko2Scale   = 173.7178
	glicko2Tau     = 0.5
	glicko2Epsilon = 0.000001
	glicko2Pi              = math.Pi
	Glicko2InactivityPeriod = 7 * 24 * time.Hour
)

// Glicko2Rating contiene la habilidad estimada, la incertidumbre y la
// volatilidad del jugador. RD alto significa que la estimación es menos segura.
type Glicko2Rating struct {
	Rating      float64
	RD          float64
	Volatility  float64
	LastRatedAt *time.Time
}

// Glicko2Opponent representa un resultado desde el punto de vista del jugador:
// score=1 victoria, score=0.5 empate, score=0 derrota.
type Glicko2Opponent struct {
	Rating      float64
	RD          float64
	Volatility  float64
	LastRatedAt *time.Time
	Score       float64
}

// DefaultGlicko2Rating devuelve los valores iniciales estándar para un jugador
// sin historial suficiente.
func DefaultGlicko2Rating() Glicko2Rating {
	return Glicko2Rating{
		Rating:     Glicko2DefaultRating,
		RD:         Glicko2DefaultRD,
		Volatility: Glicko2DefaultVolatility,
	}
}

func normalizeGlicko2(r Glicko2Rating) Glicko2Rating {
	if r.Rating == 0 || math.IsNaN(r.Rating) || math.IsInf(r.Rating, 0) {
		r.Rating = Glicko2DefaultRating
	}
	if r.RD <= 0 || math.IsNaN(r.RD) || math.IsInf(r.RD, 0) {
		r.RD = Glicko2DefaultRD
	}
	if r.Volatility <= 0 || math.IsNaN(r.Volatility) || math.IsInf(r.Volatility, 0) {
		r.Volatility = Glicko2DefaultVolatility
	}
	return r
}

// glicko2G reduce la influencia de la estimación del rival según su RD.
func glicko2G(phi float64) float64 {
	return 1 / math.Sqrt(1+3*phi*phi/(glicko2Pi*glicko2Pi))
}

func logistic(x float64) float64 {
	// Forma estable para evitar overflow en exp() cuando las diferencias son grandes.
	if x >= 0 {
		z := math.Exp(-x)
		return 1 / (1 + z)
	}
	z := math.Exp(x)
	return z / (1 + z)
}

// Glicko2At devuelve la estimación efectiva en un instante dado. Durante
// semanas sin resultados, la RD crece según la volatilidad; el rating central
// no cambia. El resultado se persiste al liquidar el siguiente set.
func Glicko2At(r Glicko2Rating, now time.Time) Glicko2Rating {
	r = normalizeGlicko2(r)
	if now.IsZero() {
		now = time.Now()
	}
	if r.LastRatedAt == nil || r.LastRatedAt.IsZero() {
		return r
	}
	elapsed := now.Sub(*r.LastRatedAt)
	if elapsed < Glicko2InactivityPeriod {
		return r
	}
	periods := int(elapsed / Glicko2InactivityPeriod)
	phi := r.RD / glicko2Scale
	phi = math.Sqrt(phi*phi + float64(periods)*r.Volatility*r.Volatility)
	r.RD = math.Min(Glicko2DefaultRD, phi*glicko2Scale)
	return r
}

// Glicko2ExpectedScore devuelve la probabilidad esperada de que a venza a b,
// utilizando el rating y RD del rival tal como define Glicko-2.
func Glicko2ExpectedScore(a, b Glicko2Rating) float64 {
	a, b = normalizeGlicko2(a), normalizeGlicko2(b)
	muA := (a.Rating - Glicko2DefaultRating) / glicko2Scale
	muB := (b.Rating - Glicko2DefaultRating) / glicko2Scale
	phiB := b.RD / glicko2Scale
	return logistic(glicko2G(phiB) * (muA - muB))
}

// Glicko2DistanceSigma mide la diferencia de rating en desviaciones estándar
// combinadas. La cola la usa para priorizar rivales con habilidad cercana,
// teniendo en cuenta la incertidumbre de ambos jugadores.
func Glicko2DistanceSigma(a, b Glicko2Rating) float64 {
	a, b = normalizeGlicko2(a), normalizeGlicko2(b)
	combinedRD := math.Hypot(a.RD, b.RD)
	if combinedRD <= 0 {
		return 0
	}
	return math.Abs(a.Rating-b.Rating) / combinedRD
}

// UpdateGlicko2Pair actualiza simultáneamente a y b usando los ratings previos
// al set de ambos. scoreA es el resultado desde la perspectiva de a.
func UpdateGlicko2Pair(a, b Glicko2Rating, scoreA float64, now time.Time) (Glicko2Rating, Glicko2Rating) {
	if scoreA < 0 {
		scoreA = 0
	}
	if scoreA > 1 {
		scoreA = 1
	}
	if now.IsZero() {
		now = time.Now()
	}
	// UpdateGlicko2 aplica inactividad al jugador y al rival una sola vez.
	// Ambos cálculos reciben los ratings pre-set, sin contaminarse entre sí.
	nextA := UpdateGlicko2(a, []Glicko2Opponent{{
		Rating: b.Rating, RD: b.RD, Volatility: b.Volatility,
		LastRatedAt: b.LastRatedAt, Score: scoreA,
	}}, now)
	nextB := UpdateGlicko2(b, []Glicko2Opponent{{
		Rating: a.Rating, RD: a.RD, Volatility: a.Volatility,
		LastRatedAt: a.LastRatedAt, Score: 1 - scoreA,
	}}, now)
	return nextA, nextB
}

// UpdateGlicko2 aplica un periodo de rating a un jugador. Los resultados de
// un periodo se calculan todos contra ratings/RD de inicio del mismo periodo.
func UpdateGlicko2(player Glicko2Rating, opponents []Glicko2Opponent, now time.Time) Glicko2Rating {
	player = Glicko2At(player, now)
	if len(opponents) == 0 {
		return player
	}
	if now.IsZero() {
		now = time.Now()
	}

	mu := (player.Rating - Glicko2DefaultRating) / glicko2Scale
	phi := player.RD / glicko2Scale

	var varianceInverse float64
	var improvementSum float64
	validResults := 0

	for _, opponent := range opponents {
		if opponent.Score < 0 || opponent.Score > 1 {
			continue
		}
		opponentRating := Glicko2At(Glicko2Rating{
			Rating:      opponent.Rating,
			RD:          opponent.RD,
			Volatility:  opponent.Volatility,
			LastRatedAt: opponent.LastRatedAt,
		}, now)
		opponentMu := (opponentRating.Rating - Glicko2DefaultRating) / glicko2Scale
		opponentPhi := opponentRating.RD / glicko2Scale
		g := glicko2G(opponentPhi)
		expected := logistic(g * (mu - opponentMu))
		varianceInverse += g * g * expected * (1 - expected)
		improvementSum += g * (opponent.Score - expected)
		validResults++
	}
	if validResults == 0 || varianceInverse <= 0 {
		return player
	}

	v := 1 / varianceInverse
	delta := v * improvementSum
	sigmaPrime := glicko2NewVolatility(phi, player.Volatility, delta, v)
	phiStar := math.Sqrt(phi*phi + sigmaPrime*sigmaPrime)
	phiPrime := 1 / math.Sqrt(1/(phiStar*phiStar)+1/v)
	muPrime := mu + phiPrime*phiPrime*improvementSum

	player.Rating = Glicko2DefaultRating + glicko2Scale*muPrime
	player.RD = glicko2Scale * phiPrime
	player.Volatility = sigmaPrime
	ratedAt := now.UTC()
	player.LastRatedAt = &ratedAt
	return player
}

// glicko2NewVolatility resuelve iterativamente la ecuación de volatilidad
// del paso 5 del algoritmo de Glickman.
func glicko2NewVolatility(phi, sigma, delta, v float64) float64 {
	a := math.Log(sigma * sigma)
	deltaSquared := delta * delta
	phiSquared := phi * phi

	f := func(x float64) float64 {
		ex := math.Exp(x)
		denominator := phiSquared + v + ex
		return ex*(deltaSquared-phiSquared-v-ex)/(2*denominator*denominator) -
			(x-a)/(glicko2Tau*glicko2Tau)
	}

	A := a
	var B float64
	if deltaSquared > phiSquared+v {
		B = math.Log(deltaSquared - phiSquared - v)
	} else {
		k := 1
		for f(a-float64(k)*glicko2Tau) < 0 && k < 100 {
			k++
		}
		B = a - float64(k)*glicko2Tau
	}

	fA, fB := f(A), f(B)
	for math.Abs(B-A) > glicko2Epsilon {
		denominator := fB - fA
		if denominator == 0 || math.IsNaN(denominator) {
			break
		}
		C := A + (A-B)*fA/denominator
		fC := f(C)
		if fC*fB < 0 {
			A, fA = B, fB
		} else {
			fA /= 2
		}
		B, fB = C, fC
	}

	result := math.Exp(A / 2)
	if result <= 0 || math.IsNaN(result) || math.IsInf(result, 0) {
		return sigma
	}
	return result
}
