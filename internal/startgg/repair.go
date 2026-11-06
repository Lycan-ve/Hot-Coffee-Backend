package startgg

import (
	"log"

	"gorm.io/gorm"
)

// RepairRankingFromPlacements reconstruye el ranking de torneos a partir de
// tournament_placements, que es la fuente de verdad que muestra el perfil web
// (la lista de torneos de cada jugador). Elimina dobles sumas históricas en
// player_results y recalcula players.points, de modo que el ranking SIEMPRE
// coincide con la suma de los torneos listados en el perfil de cada jugador.
func RepairRankingFromPlacements(db *gorm.DB) (int64, error) {

	// 1. player_results se reconstruye desde las posiciones de torneo: el
	//    mejor puntaje por jugador y evento. Un jugador con dos cuentas de
	//    Start.gg que comparten Discord queda como una sola entrada.
	if err := db.Exec(
		"DELETE FROM player_results WHERE league = ?", leagueType,
	).Error; err != nil {
		return 0, err
	}

	inserted := db.Exec(
		"INSERT INTO player_results (player_id, league, event_id, points) "+
			"SELECT player_id, league, event_id, MAX(points) "+
			"FROM tournament_placements "+
			"WHERE league = ? AND points > 0 AND player_id <> 0 "+
			"GROUP BY player_id, league, event_id", leagueType,
	)
	if inserted.Error != nil {
		return 0, inserted.Error
	}

	// 2. players.points vuelve a derivarse de player_results (la suma de los
	//    torneos del jugador), en vez de acumular incrementos.
	if err := db.Exec(
		"UPDATE players SET points = 0 WHERE league = ?", leagueType,
	).Error; err != nil {
		return inserted.RowsAffected, err
	}
	if err := db.Exec(
		"UPDATE players p SET points = sub.total "+
			"FROM (SELECT player_id, SUM(points) AS total "+
				"FROM player_results WHERE league = ? GROUP BY player_id) sub "+
			"WHERE p.id = sub.player_id AND p.league = ?", leagueType, leagueType,
	).Error; err != nil {
		return inserted.RowsAffected, err
	}

	log.Printf("🔧 Ranking reconstruido desde el historial de torneos: %d resultados verificados.", inserted.RowsAffected)
	return inserted.RowsAffected, nil
}
