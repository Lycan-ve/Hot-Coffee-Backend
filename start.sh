#!/bin/sh
# En buildpacks los binarios están en PATH; en docker están en ./ 
if command -v api >/dev/null 2>&1; then
  API=api; BOT=bot
else
  API=./api; BOT=./bot
fi

case "${SERVICE:-all}" in
  api) exec $API ;;
  bot) exec $BOT ;;
  all)
    PORT=8081 $BOT &   # el bot en segundo plano, otro puerto
    exec $API          # la API en foreground (escucha el 8080)
    ;;
esac