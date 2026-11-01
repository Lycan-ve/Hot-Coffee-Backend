#!/bin/sh
echo "==== DIAGNOSTICO DE ARRANQUE ===="
[ -n "$DATABASE_URL" ]      && echo "DATABASE_URL: OK"      || echo "DATABASE_URL: FALTA"
[ -n "$DISCORD_BOT_TOKEN" ] && echo "DISCORD_BOT_TOKEN: OK" || echo "DISCORD_BOT_TOKEN: FALTA"
[ -n "$DISCORD_GUILD_ID" ]  && echo "DISCORD_GUILD_ID: OK"  || echo "DISCORD_GUILD_ID: FALTA"
[ -n "$STARTGG_TOKEN" ]     && echo "STARTGG_TOKEN: OK"     || echo "STARTGG_TOKEN: FALTA (sync de torneos desactivada)"

echo "DATABASE_URL len: ${#DATABASE_URL}"
echo "DATABASE_URL start: $(printf '%.25s' "$DATABASE_URL")"

echo "==== DIAGNOSTICO DISCORD ===="
if command -v curl >/dev/null 2>&1; then
  curl -s -o /tmp/discord_diag.txt -w "Discord respondio: HTTP %{http_code}\n" \
    -H "Authorization: Bot $DISCORD_BOT_TOKEN" \
    --max-time 30 https://discord.com/api/v10/users/@me
  echo "Cuerpo: $(head -c 200 /tmp/discord_diag.txt)"
else
  echo "curl no disponible"
fi

# ============================================================
# ARRANQUE DE LOS DOS PROCESOS EN UN SOLO CONTENEDOR
# ============================================================
API_PORT="${PORT:-8080}"
BOT_PORT=$(( API_PORT + 1 ))

PORT="$API_PORT" ./api &
API_PID=$!

PORT="$BOT_PORT" ./bot &
BOT_PID=$!

echo ">> API escuchando en el puerto $API_PORT | bot en linea (health interno: $BOT_PORT)"

# Watchdog: si cualquiera de los dos procesos muere, el contenedor
# termina con error y Render lo reinicia completo (auto-healing).
trap 'kill $API_PID $BOT_PID 2>/dev/null; exit 1' TERM INT
while :; do
  sleep 5
  kill -0 "$API_PID" 2>/dev/null || { echo ">> la API murio (pid $API_PID)"; kill $API_PID $BOT_PID 2>/dev/null; exit 1; }
  kill -0 "$BOT_PID" 2>/dev/null || { echo ">> el bot murio (pid $BOT_PID)"; kill $API_PID $BOT_PID 2>/dev/null; exit 1; }
done