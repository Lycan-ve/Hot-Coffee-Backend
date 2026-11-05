# ---------- Build ----------
FROM golang:1.26-alpine AS builder
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
# Torneos manuales editables sin recompilar
COPY torneos_manuales.json ./torneos_manuales.json

# Iconos de rangos que la API sirve en /Rangos/
COPY Rangos ./Rangos

EXPOSE 8080
ENTRYPOINT ["./start.sh"]
