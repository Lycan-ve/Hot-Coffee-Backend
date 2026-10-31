#!/bin/sh
echo "==== DIAGNOSTICO DE ARRANQUE ===="
echo "SERVICE=${SERVICE:-all (default)}"
[ -n "$DATABASE_URL" ]      && echo "DATABASE_URL: OK"       || echo "DATABASE_URL: FALTA"
[ -n "$DISCORD_BOT_TOKEN" ] && echo "DISCORD_BOT_TOKEN: OK"  || echo "DISCORD_BOT_TOKEN: FALTA"
[ -n "$DISCORD_GUILD_ID" ]  && echo "DISCORD_GUILD_ID: OK"   || echo "DISCORD_GUILD_ID: FALTA"
[ -n "$STARTGG_TOKEN" ]     && echo "STARTGG_TOKEN: OK"     || echo "STARTGG_TOKEN: FALTA"
echo "PATH=$PATH"
command -v api >/dev/null 2>&1 && echo "binario api: en PATH"  || echo "binario api: NO en PATH"
command -v bot >/dev/null 2>&1 && echo "binario bot: en PATH"  || echo "binario bot: NO en PATH"

if command -v api >/dev/null 2>&1; then
  API=api; BOT=bot
else
  API=./api; BOT=./bot
fi

case "${SERVICE:-all}" in
  api) exec $API ;;
  bot) exec $BOT ;;
  all)
    PORT=8081 $BOT &
    exec $API
    ;;
esac