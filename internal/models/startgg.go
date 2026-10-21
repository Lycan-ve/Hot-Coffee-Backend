package models

type Player struct {
	ID        int    `gorm:"primaryKey;autoIncrement:false" json:"id"`
	League    string `gorm:"primaryKey;autoIncrement:false" json:"league"`
	GamerTag  string `json:"gamerTag"`
	Sponsor   string `json:"sponsor"`
	Character string `json:"character"` // Aquí guardaremos los mains extraídos del Top 8
	AvatarURL string `json:"avatarUrl"`
	Points    int    `json:"points"`
	Ranking   int    `json:"ranking" gorm:"-"`
}

type TournamentRecord struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	Slug      string `gorm:"uniqueIndex" json:"slug"`
	Name      string `json:"name"`
	Top8Image string `json:"top8Image"` // URL de la imagen de Discord del Top 8
}
