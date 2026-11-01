#!/bin/sh
echo "==== DIAGNOSTICO BOT-HOSTING ===="
[ -n "$DATABASE_URL" ]      && echo "DATABASE_URL: OK"      || echo "DATABASE_URL: FALTA"
[ -n "$DISCORD_BOT_TOKEN" ] && echo "DISCORD_BOT_TOKEN: OK" || echo "DISCORD_BOT_TOKEN: FALTA"
[ -n "$STARTGG_TOKEN" ]     && echo "STARTGG_TOKEN: OK"     || echo "STARTGG_TOKEN: FALTA"
[ -n "$DISCORD_TOURNAMENT_CHANNEL_ID" ] && echo "DISCORD_TOURNAMENT_CHANNEL_ID: OK" || echo "DISCORD_TOURNAMENT_CHANNEL_ID: FALTA"

echo "==== DIAGNOSTICO DISCORD ===="
if command -v curl >/dev/null 2>&1; then
  curl -s -o /tmp/discord_diag.txt -w "Discord respondio: HTTP %{http_code}\n" \
    -H "Authorization: Bot $DISCORD_BOT_TOKEN" \
    --max-time 30 https://discord.com/api/v10/users/@me
  echo "Cuerpo: $(head -c 200 /tmp/discord_diag.txt)"
fi

# Compila solo si el binario no existe (la mayoria de runtimes Go ya lo compilan)
if [ ! -x ./bot ]; then
  echo "Compilando el bot..."
  go build -tags netgo -ldflags '-s -w' -o bot ./cmd/bot || exit 1
fi

exec ./bot