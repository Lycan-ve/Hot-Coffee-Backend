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
	// Nacionalidad del jugador (ISO 3166-1 alpha-2, la elige en la web;
	// la bandera se muestra en el perfil y en el matchmaking).
	Country       string    `json:"country"`
	Roles         string    `json:"roles"` // IDs de roles separados por coma
	// Equipo principal (mains) elegido en la web: los 2 personajes y la fusión.
	MainChar1     string    `json:"mainChar1"` // Personaje 1 (point)
	MainChar2     string    `json:"mainChar2"` // Personaje 2 (assist)
	MainFuse      string    `json:"mainFuse"`  // Fusión
	FirstLogin    time.Time `json:"firstLogin"`
	LastLogin     time.Time `json:"lastLogin"`
}
