# ---------- Build ----------
FROM golang:1.24-alpine AS builder
# (Ajusta la versión a la que diga tu go.mod)
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bot ./cmd/bot

# ---------- Runtime ----------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /bot ./bot

# Archivos que el bot lee del disco en tiempo de ejecución
COPY Personajes ./Personajes
COPY Fusion ./Fusion
COPY "Hot Coffee _ 2XKO - TORNEOS HOT COFFEE - 📊│hot-coffee-torneos [1430651477612826624].json" ./

EXPOSE 8080
CMD ["./bot"]