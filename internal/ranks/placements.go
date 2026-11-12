package ranks

// Partidas de CLASIFICACIÓN (placements).
//
// Un jugador nuevo NO nace con rango: debe jugar sus primeros 5 sets de
// matchmaking y con el LP neto de esos 5 sets el sistema calcula el rango al
// que pertenece. Durante la clasificación no aplica la economía por rango
// (bonus por rango, rachas ni set perfecto): deltas fijos y amplificados para
// que 5 sets repartan al jugador por la escalera:
//
//	5V - 0D → +300 LP → Platino I     2V - 3D →    0 LP → Gold I
//	4V - 1D → +200 LP → Oro III       1V - 4D → −100 LP → Plata III
//	3V - 2D → +100 LP → Oro II        0V - 5D → −200 LP → Plata II
//
// Terminada la clasificación, el jugador entra a la economía normal con el
// rango obtenido y sigue sumando (o perdiendo) LP con las reglas por rango.
const (
	// Sets que hay que jugar antes de obtener rango.
	PlacementGames = 5

	// LP fijos por set de clasificación (la derrota resta).
	PlacementWin  = 60
	PlacementLoss = 40

	// Piso de LP durante la clasificación: con 0V-5D el jugador aterriza en
	// Plata II (800 pts totales); perder más no baja de ahí.
	PlacementFloor = -300
)

// IsPlacing dice si un jugador con `games` sets terminados sigue en
// clasificación (aún no obtiene rango).
func IsPlacing(games int) bool {
	return games < PlacementGames
}
