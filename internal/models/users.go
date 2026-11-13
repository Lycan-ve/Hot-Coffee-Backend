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
	// Nacionalidad del jugador. La toma la sync de Start.gg (ubicación
	// pública del perfil de Start.gg); también puede elegirse a mano en la
	// web como respaldo. La bandera se muestra en el perfil y en matchmaking.
	Country       string    `json:"country"`       // ISO 3166-1 alpha-2 (p. ej. VE)
	State         string    `json:"state"`         // ISO 3166-2 sin prefijo (p. ej. "A" en VE, "CA" en US)
	CountrySource string    `json:"countrySource"` // "startgg" (automática) o "manual" (elegida en la web)
	Roles         string    `json:"roles"` // IDs de roles separados por coma
	// Equipo principal (mains) elegido en la web: los 2 personajes y la fusión.
	MainChar1     string    `json:"mainChar1"` // Personaje 1 (point)
	MainChar2     string    `json:"mainChar2"` // Personaje 2 (assist)
	MainFuse      string    `json:"mainFuse"`  // Fusión
	FirstLogin    time.Time `json:"firstLogin"`
	LastLogin     time.Time `json:"lastLogin"`
}
