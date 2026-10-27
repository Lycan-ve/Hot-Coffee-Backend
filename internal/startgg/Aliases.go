package startgg

import (
	"log"
	"strings"

	"smash-backend/internal/models"

	"gorm.io/gorm"
)

// Start.gg player id -> ID de Discord (clic derecho > Copiar ID de usuario).
// Solo para quienes no tienen Discord vinculado visible en Start.gg.
var manualDiscordIDs = map[int]string{
	// 3410491: "123456789012345678", // Ñembo
}

// Cuentas de Start.gg duplicadas: id duplicado -> id de la cuenta real
var playerIDAliases = map[int]int{
	5118308: 5008791, // ErrezetaFnrir -> Fenrir.rz
}

// Tags escritos a mano en torneos manuales (en minúsculas) -> gamerTag real en la DB
var manualTagAliases = map[string]string{
	"pdpiu":        "Ñembo",
	"miyamura":     "Miyamura",
	"sandor":       "RND Sandor",
	"shine wizard": "ShineWizardsX",
	"vesani":       "VesaniFlow",
}

// Cuentas que no deben aparecer en el ranking
var blockedPlayerIDs = map[int]bool{
	158675: true, // "Deleted user"
}

// resolveManualPlayerID devuelve el id de la cuenta real si el tag tiene alias.
// Si no hay alias (o no se encuentra la cuenta), usa el id sintético de siempre.
func resolveManualPlayerID(tx *gorm.DB, tag string) int {
	syntheticID := hashGamerTag(tag)

	realTag, ok := manualTagAliases[strings.ToLower(tag)]
	if !ok {
		return syntheticID
	}

	var real models.Player
	err := tx.Where("league = ? AND gamer_tag ILIKE ? AND id <> ?",
		leagueType, realTag, syntheticID).First(&real).Error
	if err != nil {
		log.Printf("   ⚠️  Alias '%s' -> '%s' no encontrado; se usa un jugador aparte\n", tag, realTag)
		return syntheticID
	}
	return real.ID
}

// CleanupDuplicatePlayers borra las filas duplicadas que crearon las syncs anteriores.
// Solo borra por id exacto (los ids sintéticos de los alias y la cuenta duplicada de Start.gg),
// así que no toca a los jugadores reales ni sus personajes.
func CleanupDuplicatePlayers(db *gorm.DB) {
	for tag := range manualTagAliases {
		db.Where("league = ? AND id = ?", leagueType, hashGamerTag(tag)).Delete(&models.Player{})
	}
	for dupID := range playerIDAliases {
		db.Where("league = ? AND id = ?", leagueType, dupID).Delete(&models.Player{})
	}
	for id := range blockedPlayerIDs {
		db.Where("league = ? AND id = ?", leagueType, id).Delete(&models.Player{})
	}
}
