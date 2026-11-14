package ranks

// Estado "SIN RANGO": nadie parte con rango. Los jugadores nuevos cumplen
// PlacementGames sets de clasificación y su desempeño en ellos define el
// rango inicial; mientras tanto el display NO muestra ninguna división
// (ni Gold I ni ninguna otra), aunque los puntos ganados ya se usen por
// debajo para emparejar y liquidar.
const UnrankedLabel = "Aspirant"

// unrankedColor: gris neutro para un jugador sin rango.
const unrankedColor = "#9a9a9a"

// InfoFor envuelve FromPoints para el DISPLAY: si el jugador aún no completó
// sus sets de clasificación (games < PlacementGames) devuelve la información
// SIN RANGO (sin emblema, sin LP y sin siguiente división). El emparejamiento
// y la liquidación siguen usando FromPoints/TierIndex con los puntos reales;
// esto solo cambia lo que se muestra en la web.
func InfoFor(pointsWon, games int) Info {
	info := FromPoints(pointsWon)
	info.Placed = games >= PlacementGames
	if info.Placed {
		return info
	}
	info.Rank = UnrankedLabel
	info.Tier = ""
	info.DisplayName = UnrankedLabel
	info.IsRP = false
	info.LP = 0
	info.RP = 0
	info.LPToNext = 0
	info.Next = ""
	info.NextIsRP = false
	info.IconPath = ""
	info.Color = unrankedColor
	info.Progress = 0
	return info
}
