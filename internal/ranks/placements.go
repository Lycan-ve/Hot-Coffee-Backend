package ranks

// Partidas de CLASIFICACIÓN (placements).
//
// Un jugador nuevo empieza con 0 RP. Debe jugar sus primeros 5 sets de
// matchmaking para recibir un rango. Los resultados suman RP desde cero y
// nunca se permiten valores negativos.
//
//	5V - 0D → 300 RP → Iron III    2V - 3D →   0 RP → Aspirant
//	4V - 1D → 200 RP → Iron II     1V - 4D →   0 RP → Aspirant
//	3V - 2D → 100 RP → Iron I      0V - 5D →   0 RP → Aspirant
//
// Terminada la clasificación, el jugador continúa acumulando o perdiendo RP
// con las reglas normales de su rango.
const (
	PlacementGames = 5
	PlacementWin   = 60
	PlacementLoss  = 40
	PlacementFloor = 0
)

func IsPlacing(games int) bool {
	return games < PlacementGames
}
