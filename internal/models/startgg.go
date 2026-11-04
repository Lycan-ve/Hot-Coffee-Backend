package models

type Player struct {
	ID         int    `gorm:"primaryKey;autoIncrement:false" json:"id"`
	League     string `gorm:"primaryKey;autoIncrement:false" json:"league"`
	GamerTag   string `json:"gamerTag"`
	Sponsor    string `json:"sponsor"`
	Character  string `json:"character"`
	AvatarURL  string `json:"avatarUrl"`
	ProfileURL string `json:"profileUrl"` // URL del perfil de Start.gg
	DiscordID  string `json:"discordId"`  // 🆕 ID de Discord indexado del usuario
	Points     int    `json:"points"`
	Ranking    int    `json:"ranking" gorm:"-"`
}

type TournamentRecord struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	Slug      string `gorm:"uniqueIndex" json:"slug"`
	Name      string `json:"name"`
	Top8Image string `json:"top8Image"` // URL de la imagen de Discord del Top 8
}

type PlayerResult struct {
	ID       uint   `gorm:"primaryKey"`
	PlayerID int    `gorm:"uniqueIndex:idx_player_event"`
	League   string `gorm:"uniqueIndex:idx_player_event"`
	EventID  int    `gorm:"uniqueIndex:idx_player_event"`
	Points   int
}

// TournamentPlacement conserva el puesto completo de cada miembro en cada
// evento, incluso cuando no obtuvo puntos para el ranking general.
type TournamentPlacement struct {
	ID        uint   `gorm:"primaryKey"`
	DiscordID string `gorm:"uniqueIndex:idx_tournament_placement_player_event;index"`
	League    string `gorm:"uniqueIndex:idx_tournament_placement_player_event"`
	EventID   int    `gorm:"uniqueIndex:idx_tournament_placement_player_event"`
	PlayerID  int
	GamerTag  string
	Placement int
	Points    int
}
