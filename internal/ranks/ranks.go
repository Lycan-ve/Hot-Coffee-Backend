package ranks

import "fmt"

// RP necesarios para avanzar una división completa.
const RPPerDivision = 100

// Índices dentro de la escalera. Aspirant cubre 0-99 RP; cada división
// siguiente comienza al alcanzar el siguiente múltiplo de 100 RP.
const (
	AspirantIndex   = 0
	GMIndex         = 25 // Grandmaster desde 2.500 RP
	ChallengerIndex = 26 // Challenger desde 2.600 RP
)

type RankDef struct {
	Rank string
	Tier string
}

var Ladder = []RankDef{
	{"Aspirant", ""},
	{"Iron", "I"}, {"Iron", "II"}, {"Iron", "III"},
	{"Bronze", "I"}, {"Bronze", "II"}, {"Bronze", "III"},
	{"Silver", "I"}, {"Silver", "II"}, {"Silver", "III"},
	{"Gold", "I"}, {"Gold", "II"}, {"Gold", "III"},
	{"Platinum", "I"}, {"Platinum", "II"}, {"Platinum", "III"},
	{"Emerald", "I"}, {"Emerald", "II"}, {"Emerald", "III"},
	{"Diamond", "I"}, {"Diamond", "II"}, {"Diamond", "III"},
	{"Master", "I"}, {"Master", "II"}, {"Master", "III"},
	{"Grandmaster", ""},
	{"Challenger", ""},
}

var Colors = map[string]string{
	"Aspirant": "#b0a89e", "Iron": "#c8c8c8", "Bronze": "#e8a065", "Silver": "#c0d5cc",
	"Gold": "#f5d060", "Platinum": "#78d8f0", "Emerald": "#60e090",
	"Diamond": "#90b8ff", "Master": "#d080f8", "Grandmaster": "#f07070",
	"Challenger": "#ffe787",
}

var tierToIdx = map[string]int{"I": 1, "II": 2, "III": 3}

type Info struct {
	Rank          string  `json:"rank"`
	Tier          string  `json:"tier"`
	DisplayName   string  `json:"displayName"`
	IsRP          bool    `json:"isRp"` // true para GM/Challenger, que no tienen tier
	RP            int     `json:"rp"` // total de puntos de ranking; todos parten de 0
	Points        int     `json:"points"` // alias compatible de RP total
	RPToNext      int     `json:"rpToNext"`
	Next          string  `json:"next"`
	NextIsRP      bool    `json:"nextIsRp"`
	DivisionIndex int     `json:"divisionIndex"`
	Progress      float64 `json:"progress"`
	IconPath      string  `json:"iconPath"`
	Color         string  `json:"color"`
	Placed        bool    `json:"placed"`
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

// FromPoints calcula rango y RP directamente desde los puntos ganados.
// No hay base oculta: 0 RP = Aspirant, 100 = Iron I, 1.000 = Gold I,
// 2.500 = Grandmaster y 2.600 = Challenger.
func FromPoints(points int) Info {
	total := points
	if total < 0 {
		total = 0
	}
	idx := total / RPPerDivision
	if idx >= len(Ladder) {
		idx = len(Ladder) - 1
	}
	d := Ladder[idx]
	info := Info{
		Rank:          d.Rank,
		Tier:          d.Tier,
		DisplayName:   d.displayName(),
		IsRP:          idx >= GMIndex,
		RP:            total,
		Points:        total,
		DivisionIndex: idx,
		IconPath:      iconPath(d),
		Color:         Colors[d.Rank],
	}

	if info.IsRP {
		info.Progress = 100
		if idx < len(Ladder)-1 {
			info.Next = Ladder[idx+1].displayName()
			info.RPToNext = (idx+1)*RPPerDivision - total
			info.NextIsRP = idx+1 >= GMIndex
		}
		return info
	}

	info.RPToNext = RPPerDivision - (total % RPPerDivision)
	if idx+1 < len(Ladder) {
		info.Next = Ladder[idx+1].displayName()
		info.NextIsRP = idx+1 >= GMIndex
	}
	info.Progress = float64(total%RPPerDivision) / float64(RPPerDivision) * 100
	return info
}
