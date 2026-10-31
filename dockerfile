# ---------- Build ----------
FROM golang:1.26-alpine AS builder
# (go.mod exige 1.26.5; el 1.24 que tenías es anterior)
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bot ./cmd/bot && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /api ./cmd/api

# ---------- Runtime ----------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /bot ./bot
COPY --from=builder /api ./api
COPY start.sh ./start.sh
RUN chmod +x start.sh

# Archivos que el bot lee del disco en tiempo de ejecución
COPY Personajes ./Personajes
COPY Fusion ./Fusion
COPY "Hot Coffee _ 2XKO - TORNEOS HOT COFFEE - 📊│hot-coffee-torneos [1430651477612826624].json" ./

EXPOSE 8080
ENTRYPOINT ["./start.sh"]