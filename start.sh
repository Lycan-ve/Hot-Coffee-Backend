#!/bin/sh
echo "==== DIAGNOSTICO DE ARRANQUE ===="
[ -n "$DATABASE_URL" ]   && echo "DATABASE_URL: OK"   || echo "DATABASE_URL: FALTA"
[ -n "$DISCORD_BOT_TOKEN" ] && echo "DISCORD_BOT_TOKEN: OK" || echo "DISCORD_BOT_TOKEN: FALTA"
[ -n "$DISCORD_GUILD_ID" ]  && echo "DISCORD_GUILD_ID: OK"  || echo "DISCORD_GUILD_ID: FALTA"

# ← LAS DOS LÍNEAS CLAVE
echo "DATABASE_URL len: ${#DATABASE_URL}"
echo "DATABASE_URL start: $(printf '%.25s' "$DATABASE_URL")"

if command -v bot >/dev/null 2>&1; then
  exec bot
else
  exec ./bot
fi