package models

type Player struct {
	ID        int    `gorm:"primaryKey;autoIncrement:false" json:"id"`
	League    string `gorm:"primaryKey;autoIncrement:false" json:"league"`
	GamerTag  string `json:"gamerTag"`
	Sponsor   string `json:"sponsor"`
	Character string `json:"character"`
	AvatarURL string `json:"avatarUrl"`
	Points    int    `json:"points"`
	Ranking   int    `json:"ranking" gorm:"-"`
}
