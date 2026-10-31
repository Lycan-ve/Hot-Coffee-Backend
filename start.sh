#!/bin/sh
case "${SERVICE:-all}" in
  api) exec ./api ;;
  bot) exec ./bot ;;
  all)
    # Ambos en un contenedor: el bot usa otro puerto para no chocar con la API
    PORT=8081 ./bot &
    exec ./api
    ;;
esac