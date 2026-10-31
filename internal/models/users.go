package models

import "time"

// User es un usuario de la web autenticado con Discord OAuth2.
// Solo puede existir si es miembro del servidor de Hot Coffee 2XKO
// (el check se hace en el callback del login, con el scope guilds.members.read).
type User struct {
	DiscordID     string    `gorm:"primaryKey" json:"discordId"`
	Username      string    `json:"username"`      // nombre único de Discord
	GlobalName    string    `json:"globalName"`    // nombre visible global
	GuildNickname string    `json:"guildNickname"` // apodo dentro del servidor
	AvatarURL     string    `json:"avatarUrl"`
	Roles         string    `json:"roles"` // IDs de roles separados por coma
	FirstLogin    time.Time `json:"firstLogin"`
	LastLogin     time.Time `json:"lastLogin"`
}
