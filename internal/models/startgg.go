package models

type GraphQLPayload struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

type StartGGResponse struct {
	Data struct {
		Tournaments struct {
			Nodes []TournamentNode `json:"nodes"`
		} `json:"tournaments"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type AdminNode struct {
	Name     string `json:"name"`
	GamerTag string `json:"gamerTag"`
}

type TournamentNode struct {
	ID       int         `json:"id"`
	Name     string      `json:"name"`
	Slug     string      `json:"slug"`
	IsOnline bool        `json:"isOnline"`
	Admins   []AdminNode `json:"admins"`
	Events   []EventNode `json:"events"`
}

type EventNode struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Videogame struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"videogame"`
	Standings struct {
		Nodes []StandingNode `json:"nodes"`
	} `json:"standings"`
}

// ⚠️ Se ajusta GamerTag dentro de Participants (Participant Type en Start.gg)
type StandingNode struct {
	Placement int `json:"placement"`
	Entrant   struct {
		Name         string `json:"name"`
		Participants []struct {
			GamerTag string `json:"gamerTag"` // <- Ahora mapeado correctamente desde Participant
			Player   struct {
				ID int `json:"id"`
			} `json:"player"`
		} `json:"participants"`
	} `json:"entrant"`
}

type PlayerRanking struct {
	ID            int    `json:"id"`
	Rank          int    `json:"rank"`
	Name          string `json:"name"`
	MainCharacter string `json:"mainCharacter"`
	League        string `json:"league"`
	Points        int    `json:"points"`
}
