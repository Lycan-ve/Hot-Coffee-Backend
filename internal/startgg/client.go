package startgg

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"smash-backend/internal/models" // Cambia "your_project_module" por el nombre de tu módulo en go.mod

	"gorm.io/gorm"
)

type Attachment struct {
	URL      string `json:"url"`
	FileName string `json:"fileName"`
}

type ForwardedMessage struct {
	Content     string       `json:"content"`
	Attachments []Attachment `json:"attachments"`
}

type DiscordMessage struct {
	Content          string            `json:"content"`
	Attachments      []Attachment      `json:"attachments"`
	ForwardedMessage *ForwardedMessage `json:"forwardedMessage"`
}

type DiscordExport struct {
	Messages []DiscordMessage `json:"messages"`
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

type TournamentInfo struct {
	Slug       string
	Top8ImgURL string
}

func ExtractTournamentsAndImages(messages []DiscordMessage) []TournamentInfo {
	tournamentMap := make(map[string]string)
	re := regexp.MustCompile(`start\.gg/tournament/([a-zA-Z0-9\-_]+)`)

	for _, msg := range messages {
		content := msg.Content
		var attachments []Attachment = msg.Attachments

		if msg.ForwardedMessage != nil {
			content += " " + msg.ForwardedMessage.Content
			attachments = append(attachments, msg.ForwardedMessage.Attachments...)
		}

		matches := re.FindAllStringSubmatch(content, -1)
		var imgURL string
		if len(attachments) > 0 {
			imgURL = attachments[0].URL
		}

		for _, match := range matches {
			if len(match) > 1 {
				slug := match[1]
				tournamentMap[slug] = imgURL
			}
		}
	}

	var results []TournamentInfo
	for slug, img := range tournamentMap {
		results = append(results, TournamentInfo{Slug: slug, Top8ImgURL: img})
	}
	return results
}

func SyncTournamentsFromDiscordJSON(filePath string, apiToken string, db *gorm.DB) {
	file, err := os.Open(filePath)
	if err != nil {
		log.Printf("❌ Error al abrir el archivo de Discord: %v\n", err)
		return
	}
	defer file.Close()

	var exportData DiscordExport
	if err := json.NewDecoder(file).Decode(&exportData); err != nil {
		log.Printf("❌ Error al decodificar el JSON: %v\n", err)
		return
	}

	tournaments := ExtractTournamentsAndImages(exportData.Messages)
	log.Printf("🔍 Se encontraron %d torneos únicos en el canal de Discord.\n", len(tournaments))

	client := &http.Client{Timeout: 30 * time.Second}
	leagueType := "2xko_online"

	for _, t := range tournaments {
		log.Printf("📥 Procesando torneo Start.gg [Slug: %s]\n", t.Slug)

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

					var player models.Player
					err := db.First(&player, "id = ? AND league = ?", playerID, leagueType).Error

					if err != nil {
						player = models.Player{
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
	log.Printf("✅ Sincronización completa con los torneos de Discord.\n")
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
