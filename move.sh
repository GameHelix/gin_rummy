#!/usr/bin/env bash
# Moves the engine to a server prepared by candylario/infra: deploys a fresh
# stack there, stops the engine here, copies the database over and starts
# it there. Nothing keeps running here afterwards, so no account is ever
# traded from two places; the local copy stays as a backup.
#
#   ./move.sh deploy@SERVER
# shellcheck disable=SC2029 # $dir is meant to expand here, before ssh.
set -euo pipefail
host=${1:?usage: ./move.sh deploy@SERVER}
dir=/srv/trader
cd "$(dirname "$0")"

./deploy.sh "$host"
ssh "$host" "cd $dir && docker compose stop trader"
docker compose stop trader
echo "Copying the database..."
docker compose exec -T postgres pg_dump -U trader -d trader --clean --if-exists | gzip |
  ssh "$host" "cd $dir && gunzip | docker compose exec -T postgres psql -q -v ON_ERROR_STOP=1 -U trader -d trader" >/dev/null
ssh "$host" "cd $dir && docker compose up -d --no-build"
docker compose down
echo "The engine now runs on $host; nothing runs here. Logs: ssh $host 'cd $dir && docker compose logs -f trader'"
