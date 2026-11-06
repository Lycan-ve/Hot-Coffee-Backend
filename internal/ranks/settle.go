package ranks

// Reglas de puntos del matchmaking equitativo (FT 3).
//
// Dentro de un mismo rango, las divisiones (I, II y III) son el MISMO nivel:
// Gold I, Gold II y Gold III se emparejan entre sí y valen los mismos puntos.
// Vencer a un rango MÁS ALTO da más puntos; caer ante uno más bajo duele más.
const (
	WinPointsSameTier  = 15 // victoria contra tu mismo nivel
	LosePointsSameTier = 5  // derrota contra tu mismo nivel
	WinBonusPerTier    = 10 // bonus por cada nivel MÁS ALTO del rival vencido
	LosePenaltyPerTier = 5  // castigo extra por cada nivel MÁS BAJO del rival
	MinPointsFloor     = 5  // piso: nadie baja de 5 puntos ganados
	StreakForUpperTier = 3  // racha de victorias que habilita rival de un rango arriba
)

// TierOrder: niveles de la escalera, de menor a mayor.
var TierOrder = []string{
	"Iron", "Bronze", "Silver", "Gold", "Platinum", "Emerald",
	"Diamond", "Master", "Grandmaster", "Challenger",
}

// TierIndex devuelve el NIVEL (0 = Iron ... 9 = Challenger) de un jugador
// según sus puntos GANADOS en matchmaking (los que guarda el ranking).
// Todas las divisiones de un rango comparten nivel (Gold I = Gold II = Gold III).
func TierIndex(pointsWon int) int {
	rank := FromPoints(pointsWon).Rank
	for i, t := range TierOrder {
		if t == rank {
			return i
		}
	}
	return 0
}

// WinPoints: puntos que gana el GANADOR. Recibe los puntos ganados de ambos
// (winner y loser). Mismo nivel: valor base. Vencer a un nivel superior:
// +WinBonusPerTier por cada nivel de diferencia. Vencer a un nivel inferior:
// solo el valor base.
func WinPoints(winnerPoints, loserPoints int) int {
	diff := TierIndex(loserPoints) - TierIndex(winnerPoints)
	if diff < 0 {
		diff = 0
	}
	return WinPointsSameTier + WinBonusPerTier*diff
}

// LosePoints: puntos que pierde el PERDEDOR (positivo; el llamador le aplica
// el piso de MinPointsFloor). Caer ante un nivel inferior cuesta más:
// +LosePenaltyPerTier por cada nivel que el perdedor esté por encima del ganador.
func LosePoints(winnerPoints, loserPoints int) int {
	diff := TierIndex(loserPoints) - TierIndex(winnerPoints)
	if diff < 0 {
		diff = 0
	}
	return LosePointsSameTier + LosePenaltyPerTier*diff
}

// MaxStakeOf estima los puntos en juego de un emparejamiento: lo máximo que
// puede ganar es el jugador del nivel más bajo (si vence hacia arriba).
func MaxStakeOf(pointsA, pointsB int) int {
	lo, hi := pointsA, pointsB
	if lo > hi {
		lo, hi = hi, lo
	}
	return WinPoints(lo, hi)
}
