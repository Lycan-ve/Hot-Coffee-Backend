package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Player struct {
	ID        int    `gorm:"primaryKey;autoIncrement:false" json:"id"`
	League    string `gorm:"primaryKey;autoIncrement:false" json:"league"`
	GamerTag  string `json:"gamerTag"`
	Sponsor   string `json:"sponsor"`
	Character string `json:"character"`
	AvatarURL string `json:"avatarUrl"` // Campo para la foto de perfil de Start.gg
	Points    int    `json:"points"`
	Ranking   int    `json:"ranking" gorm:"-"`
}

var db *gorm.DB

type UserTournamentsList struct {
	Data struct {
		User struct {
			Tournaments struct {
				Nodes []struct {
					ID   int    `json:"id"`
					Name string `json:"name"`
					Slug string `json:"slug"`
				} `json:"nodes"`
			} `json:"tournaments"`
		} `json:"user"`
	} `json:"data"`
}

type TournamentDetail struct {
	Data struct {
		Tournament struct {
			Name   string `json:"name"`
			Events []struct {
				ID        int    `json:"id"`
				Name      string `json:"name"`
				Videogame struct {
					ID   int    `json:"id"`
					Name string `json:"name"`
				} `json:"videogame"`
				Standings struct {
					Nodes []struct {
						Placement int `json:"placement"`
						Entrant   struct {
							Name         string `json:"name"`
							Participants []struct {
								Player struct {
									ID   int `json:"id"`
									User struct {
										Images []struct {
											Type string `json:"type"`
											URL  string `json:"url"`
										} `json:"images"`
									} `json:"user"`
								} `json:"player"`
							} `json:"participants"`
						} `json:"entrant"`
					} `json:"nodes"`
				} `json:"standings"`
			} `json:"events"`
		} `json:"tournament"`
	} `json:"data"`
}

func InitDatabase() {
	dsn := "host=localhost user=postgres password=root dbname=power_ranking port=5432 sslmode=disable TimeZone=America/Caracas"
	var err error
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		log.Fatal("Error al conectar a la base de datos:", err)
	}

	db.AutoMigrate(&Player{})
	fmt.Println("Base de datos conectada y migrada correctamente con fotos de perfil.")
}

func SyncHotCoffeeTournaments(apiToken string) {
	log.Printf("🔍 Obteniendo lista de torneos...\n")
	client := &http.Client{Timeout: 30 * time.Second}

	listQuery := `
		query UserTournaments($slug: String!) {
			user(slug: $slug) {
				tournaments(query: { perPage: 50, page: 1 }) {
					nodes {
						id
						name
						slug
					}
				}
			}
		}
	`

	reqBody, _ := json.Marshal(map[string]interface{}{
		"query": listQuery,
		"variables": map[string]interface{}{
			"slug": "4581ad07",
		},
	})

	req, err := http.NewRequest("POST", "https://api.start.gg/gql/alpha", bytes.NewBuffer(reqBody))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	var listResult UserTournamentsList
	json.Unmarshal(bodyBytes, &listResult)

	tournaments := listResult.Data.User.Tournaments.Nodes

	for _, t := range tournaments {
		tNameLower := strings.ToLower(t.Name)
		tSlugLower := strings.ToLower(t.Slug)

		if !(strings.Contains(tNameLower, "hot coffee") || strings.Contains(tSlugLower, "hot-coffee")) {
			continue
		}
		if strings.Contains(tNameLower, "sf6") || strings.Contains(tSlugLower, "sf6") {
			continue
		}

		// Consulta detallada solicitando imágenes de avatar de los usuarios
		detailQuery := `
			query TournamentDetails($slug: String!) {
				tournament(slug: $slug) {
					name
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
											user {
												images {
													type
													url
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		`

		detailBody, _ := json.Marshal(map[string]interface{}{
			"query": detailQuery,
			"variables": map[string]interface{}{
				"slug": t.Slug,
			},
		})

		detailReq, _ := http.NewRequest("POST", "https://api.start.gg/gql/alpha", bytes.NewBuffer(detailBody))
		detailReq.Header.Set("Authorization", "Bearer "+apiToken)
		detailReq.Header.Set("Content-Type", "application/json")

		detailResp, err := client.Do(detailReq)
		if err != nil {
			continue
		}

		detailBytes, _ := io.ReadAll(detailResp.Body)
		detailResp.Body.Close()

		var detailResult TournamentDetail
		if json.Unmarshal(detailBytes, &detailResult) != nil {
			continue
		}

		leagueType := "2xko_online"

		for _, event := range detailResult.Data.Tournament.Events {
			vgName := strings.ToLower(event.Videogame.Name)
			if strings.Contains(vgName, "street fighter") || strings.Contains(vgName, "sf6") {
				continue
			}

			for _, node := range event.Standings.Nodes {
				earnedPoints := calculatePoints(node.Placement)
				if earnedPoints == 0 {
					continue
				}

				rawName := node.Entrant.Name
				sponsor := ""
				gamerTag := rawName

				if strings.Contains(rawName, " | ") {
					parts := strings.SplitN(rawName, " | ", 2)
					sponsor = parts[0]
					gamerTag = parts[1]
				}

				for _, participant := range node.Entrant.Participants {
					playerID := participant.Player.ID
					if playerID == 0 {
						continue
					}

					// Extraer la foto de perfil (profile / avatar)
					avatarURL := ""
					for _, img := range participant.Player.User.Images {
						if img.Type == "profile" {
							avatarURL = img.URL
							break
						}
					}
					if avatarURL == "" && len(participant.Player.User.Images) > 0 {
						avatarURL = participant.Player.User.Images[0].URL
					}

					var player Player
					err := db.First(&player, "id = ? AND league = ?", playerID, leagueType).Error

					if err != nil {
						player = Player{
							ID:        playerID,
							League:    leagueType,
							GamerTag:  gamerTag,
							Sponsor:   sponsor,
							Character: "2XKO_FIGHTER",
							AvatarURL: avatarURL,
							Points:    earnedPoints,
						}
						db.Create(&player)
					} else {
						player.Points += earnedPoints
						player.GamerTag = gamerTag
						if sponsor != "" {
							player.Sponsor = sponsor
						}
						if avatarURL != "" {
							player.AvatarURL = avatarURL
						}
						db.Save(&player)
					}
				}
			}
		}
	}
	log.Printf("✅ Sincronización completa con fotos de perfil.\n")
}

func calculatePoints(placement int) int {
	switch {
	case placement == 1:
		return 100
	case placement == 2:
		return 70
	case placement == 3:
		return 50
	case placement == 4:
		return 35
	case placement >= 5 && placement <= 6:
		return 20
	case placement >= 7 && placement <= 8:
		return 10
	case placement >= 9 && placement <= 12:
		return 5
	case placement >= 13 && placement <= 16:
		return 3
	case placement > 16:
		return 1
	default:
		return 0
	}
}

func HandleGetRankings(w http.ResponseWriter, r *http.Request, league string) {
	var players []Player
	db.Where("league = ?", league).Order("points DESC").Find(&players)

	for i := range players {
		players[i].Ranking = i + 1
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(players)
}

func main() {
	godotenv.Load()
	InitDatabase()

	startGGToken := os.Getenv("STARTGG_TOKEN")

	go func() {
		if startGGToken != "" {
			SyncHotCoffeeTournaments(startGGToken)
		}
	}()

	http.HandleFunc("/api/rankings/2xko", func(w http.ResponseWriter, r *http.Request) {
		HandleGetRankings(w, r, "2xko_online")
	})

	http.HandleFunc("/api/sync", func(w http.ResponseWriter, r *http.Request) {
		if startGGToken != "" {
			SyncHotCoffeeTournaments(startGGToken)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write([]byte(`{"status": "success", "message": "Sincronizado con éxito con fotos de Start.gg."}`))
	})

	port := ":8080"
	fmt.Printf("Servidor corriendo en http://localhost%s\n", port)
	log.Fatal(http.ListenAndServe(port, nil))
}
