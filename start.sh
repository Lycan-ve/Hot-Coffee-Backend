#!/bin/sh
# Arranca el proceso elegido con RUN_MODE: "api" (por defecto) o "bot".
set -e

if [ "${RUN_MODE:-api}" = "bot" ]; then
  echo "==== Arrancando BOT de Discord (RUN_MODE=bot) ===="
  exec ./bot
fi

echo "==== Arrancando API web (RUN_MODE=api) ===="
exec ./api
