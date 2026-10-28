package gamechars

import "fmt"

// Character es un personaje jugable de 2XKO.
type Character struct {
	ID   string // ID = nombre base del archivo en Personajes/ (sin prefijo/postfix)
	Name string // nombre visible en menús y paneles
}

// Fuse es una fusión de equipo de 2XKO.
type Fuse struct {
	ID   string
	Name string
}

// Roster de 2XKO: coincide con los archivos de Personajes/ y Fusion/ del repo.
var Characters = []Character{
	{"Ahri", "Ahri"}, {"Akali", "Akali"}, {"Blitzcrank", "Blitzcrank"},
	{"Braum", "Braum"}, {"Caitlyn", "Caitlyn"}, {"Darius", "Darius"},
	{"Ekko", "Ekko"}, {"Illaoi", "Illaoi"}, {"Jinx", "Jinx"}, {"Lux", "Lux"},
	{"Senna", "Senna"}, {"Teemo", "Teemo"}, {"Thresh", "Thresh"}, {"Vi", "Vi"},
	{"Warwick", "Warwick"}, {"Yasuo", "Yasuo"},
}

var Fuses = []Fuse{
	{"2XAssist", "2X Assist"}, {"DoubleDown", "Double Down"}, {"Freestyle", "Freestyle"},
	{"Juggernaut", "Juggernaut"}, {"Sidekick", "Sidekick"}, {"Teamfight", "Teamfight"},
}

// CharacterName / FuseName devuelven el nombre visible; "" si el ID no existe.
func CharacterName(id string) string {
	for _, c := range Characters {
		if c.ID == id {
			return c.Name
		}
	}
	return ""
}

func FuseName(id string) string {
	for _, f := range Fuses {
		if f.ID == id {
			return f.Name
		}
	}
	return ""
}

// ValidCharacter / ValidFuse validan IDs llegados desde los menús de Discord.
func ValidCharacter(id string) bool { return CharacterName(id) != "" }
func ValidFuse(id string) bool      { return FuseName(id) != "" }

// TeamLabel arma "Ahri + Ekko · 🔀 Sidekick" (Personaje 1 + Personaje 2 · Fusión).
// Lo que falta sale como "—".
func TeamLabel(c1, c2, fuse string) string {
	ch := func(id string) string {
		if n := CharacterName(id); n != "" {
			return n
		}
		return "—"
	}
	f := "—"
	if n := FuseName(fuse); n != "" {
		f = n
	}
	return fmt.Sprintf("%s + %s · 🔀 %s", ch(c1), ch(c2), f)
}

// Rutas de los iconos dentro del proyecto (para adjuntarlos por attachment:// más adelante).
func CharacterIcon(id string) string { return fmt.Sprintf("Personajes/file_%s_0.png", id) }
func FuseIcon(id string) string      { return fmt.Sprintf("Fusion/file_%sV.png", id) }
