package ranks

import "fmt"

// LP necesarios para subir una división completa (tier).
const LPPerDivision = 100

// Índices dentro de la escalera de los rangos sin tier.
const (
	GMIndex         = 24 // Grandmaster a los 2400 puntos
	ChallengerIndex = 25 // Challenger a los 2500 puntos
)

// RankDef es una división de la escalera (rango + tier).
// Grandmaster y Challenger no tienen tier (van por RP).
type RankDef struct {
	Rank string
	Tier string // "III", "II", "I" o "" para GM/Challenger
}

// Ladder completa: el índice i se alcanza con i*100 puntos.
var Ladder = []RankDef{
	{"Iron", "III"}, {"Iron", "II"}, {"Iron", "I"},
	{"Bronze", "III"}, {"Bronze", "II"}, {"Bronze", "I"},
	{"Silver", "III"}, {"Silver", "II"}, {"Silver", "I"},
	{"Gold", "III"}, {"Gold", "II"}, {"Gold", "I"},
	{"Platinum", "III"}, {"Platinum", "II"}, {"Platinum", "I"},
	{"Emerald", "III"}, {"Emerald", "II"}, {"Emerald", "I"},
	{"Diamond", "III"}, {"Diamond", "II"}, {"Diamond", "I"},
	{"Master", "III"}, {"Master", "II"}, {"Master", "I"},
	{"Grandmaster", ""},
	{"Challenger", ""},
}

// Colors: los mismos colores que usan los overlays.
var Colors = map[string]string{
	"Iron": "#c8c8c8", "Bronze": "#e8a065", "Silver": "#c0d5cc",
	"Gold": "#f5d060", "Platinum": "#78d8f0", "Emerald": "#60e090",
	"Diamond": "#90b8ff", "Master": "#d080f8", "Grandmaster": "#f07070",
	"Challenger": "#ffe787",
}

var tierToIdx = map[string]int{"I": 1, "II": 2, "III": 3}

// Info es todo lo que el frontend necesita para pintar el rank display.
type Info struct {
	Rank          string  `json:"rank"`
	Tier          string  `json:"tier"`
	DisplayName   string  `json:"displayName"` // "Gold II"
	IsRP          bool    `json:"isRp"`        // true para GM/Challenger
	LP            int     `json:"lp"`          // 0-99 (rangos con tier)
	RP            int     `json:"rp"`          // puntos totales (GM/Challenger)
	Points        int     `json:"points"`      // puntos del ranking de matchmaking
	LPToNext      int     `json:"lpToNext"`    // cuánto falta para subir
	Next          string  `json:"next"`        // siguiente división, "" si ya es Challenger
	NextIsRP      bool    `json:"nextIsRp"`
	DivisionIndex int     `json:"divisionIndex"`
	Progress      float64 `json:"progress"` // 0-100 % para la barra
	IconPath      string  `json:"iconPath"` // p. ej. "Rangos/Gold_tier_2_icon.png"
	Color         string  `json:"color"`
}

func (d RankDef) displayName() string {
	if d.Tier == "" {
		return d.Rank
	}
	return d.Rank + " " + d.Tier
}

func iconPath(d RankDef) string {
	if d.Tier == "" {
		return "Rangos/" + d.Rank + "_icon.png"
	}
	return fmt.Sprintf("Rangos/%s_tier_%d_icon.png", d.Rank, tierToIdx[d.Tier])
}

// FromPoints calcula el rango actual a partir de los puntos del ranking
// de matchmaking. Cada 100 puntos se sube una división; al llegar al
// índice 24 (2400 pts) eres Grandmaster y al 25 (2500 pts) Challenger.
// GM/Challenger muestran RP total en lugar de la barra de LP.
func FromPoints(points int) Info {
	if points < 0 {
		points = 0
	}
	idx := points / LPPerDivision
	if idx >= len(Ladder) {
		idx = len(Ladder) - 1
	}
	d := Ladder[idx]
	info := Info{
		Rank:          d.Rank,
		Tier:          d.Tier,
		DisplayName:   d.displayName(),
		Points:        points,
		DivisionIndex: idx,
		IconPath:      iconPath(d),
		Color:         Colors[d.Rank],
		IsRP:          idx >= GMIndex,
	}

	if info.IsRP {
		// Grandmaster / Challenger: RP totales, sin barra de LP.
		info.RP = points
		info.Progress = 100
		if idx < len(Ladder)-1 {
			next := Ladder[idx+1]
			info.Next = next.displayName()
			info.LPToNext = (idx+1)*LPPerDivision - points
		}
		return info
	}

	// Rangos con tier (I-III): LP 0-99 dentro de la división.
	info.LP = points % LPPerDivision
	info.LPToNext = LPPerDivision - info.LP
	if idx+1 < len(Ladder) {
		info.Next = Ladder[idx+1].displayName()
		info.NextIsRP = idx+1 >= GMIndex
	}
	info.Progress = float64(info.LP) / float64(LPPerDivision) * 100
	return info
}
