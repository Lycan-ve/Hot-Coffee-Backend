package startgg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"smash-backend/internal/models"
	"strings"
)

const startGGAPI = "https://api.start.gg/gql/alpha"

// FetchHotCoffeeTournaments busca los torneos online de 2XKO a nivel nacional
// filtrando por la marca "Hot Coffee", juego "2XKO" o administración de "Baneado Sensei".
func FetchHotCoffeeTournaments() (*models.StartGGResponse, error) {
	token := os.Getenv("STARTGG_TOKEN")
	fmt.Println("Token cargado correctamente, longitud:", len(token))
	if token == "" {
		return nil, fmt.Errorf("STARTGG_TOKEN no está definido")
	}

	// Consulta GraphQL optimizada para torneos online con extracción masiva de standings (hasta 100 jugadores por evento)
	query := `
    query OnlineTournaments($perPage: Int!, $page: Int!) {
        tournaments(query: {
            perPage: $perPage,
            page: $page,
            filter: {
                isOnline: true
            }
        }) {
            nodes {
                id
                name
                slug
                isOnline
                admins {
                    name
                    gamerTag
                }
                events {
                    id
                    name
                    videogame {
                        id
                        name
                    }
                    standings(query: { perPage: 100, page: 1 }) {
                        nodes {
                            placement
                            entrant {
                                name
                                participants {
                                    player {
                                        id
                                        gamerTag
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
    }`

	payload := models.GraphQLPayload{
		Query: query,
		Variables: map[string]interface{}{
			"perPage": 50,
			"page":    1,
		},
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", startGGAPI, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var response models.StartGGResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}

	if len(response.Errors) > 0 {
		return nil, fmt.Errorf("error de la API Start.gg: %s", response.Errors[0].Message)
	}

	// Filtrado inteligente en Go para capturar Hot Coffee, 2XKO o torneos de Baneado Sensei
	var filteredTournaments []models.TournamentNode
	for _, t := range response.Data.Tournaments.Nodes {
		tNameLower := strings.ToLower(t.Name)
		tSlugLower := strings.ToLower(t.Slug)

		isHotCoffee := strings.Contains(tNameLower, "hot coffee") || strings.Contains(tSlugLower, "hot-coffee")
		is2XKO := strings.Contains(tNameLower, "2xko")

		hasBaneadoSensei := false
		for _, admin := range t.Admins {
			adminStr := strings.ToLower(admin.Name + " " + admin.GamerTag)
			if strings.Contains(adminStr, "baneado") || strings.Contains(adminStr, "sensei") {
				hasBaneadoSensei = true
				break
			}
		}

		// Si pertenece al circuito o es administrado por Baneado Sensei
		if isHotCoffee || is2XKO || hasBaneadoSensei {
			var validEvents []models.EventNode
			for _, ev := range t.Events {
				vgName := strings.ToLower(ev.Videogame.Name)
				evName := strings.ToLower(ev.Name)
				// Asegurar que el evento corresponda a 2XKO
				if strings.Contains(vgName, "2xko") || strings.Contains(evName, "2xko") {
					validEvents = append(validEvents, ev)
				}
			}

			if len(validEvents) > 0 {
				t.Events = validEvents
				filteredTournaments = append(filteredTournaments, t)
			}
		}
	}

	response.Data.Tournaments.Nodes = filteredTournaments

	return &response, nil
}
