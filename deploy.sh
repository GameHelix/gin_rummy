#!/usr/bin/env bash
# Deploys the engine to a server prepared by candylario/infra: builds the image
# for the server's CPU, copies it over SSH with compose.yaml, config/ and
# .env, and restarts the stack. The data and the database stay on the server.
#
#   ./deploy.sh deploy@SERVER
# shellcheck disable=SC2029 # $dir is meant to expand here, before ssh.
set -euo pipefail
host=${1:?usage: ./deploy.sh deploy@SERVER}
dir=/srv/trader
cd "$(dirname "$0")"
[ -f .env ] || {
  echo ".env is missing" >&2
  exit 1
}

case $(ssh "$host" uname -m) in
x86_64) platform=linux/amd64 ;;
aarch64 | arm64) platform=linux/arm64 ;;
*)
  echo "unsupported server CPU" >&2
  exit 1
  ;;
esac

docker buildx build --platform "$platform" --tag trader:deploy --load .
ssh "$host" "mkdir -p $dir/config && sudo install -d -o 65532 -g 65532 $dir/data"
docker save trader:deploy | gzip | ssh "$host" 'gunzip | docker load --quiet && docker tag trader:deploy trader:local'
scp -q compose.yaml "$host:$dir/compose.yaml"
scp -qr config/. "$host:$dir/config/"
ssh "$host" "umask 077 && cat >$dir/.env" <.env
ssh "$host" "docker network inspect candylario >/dev/null 2>&1 || docker network create candylario >/dev/null"
ssh "$host" "cd $dir && docker compose up -d --no-build --remove-orphans && docker image prune -f >/dev/null"
echo "Deployed to $host. Logs: ssh $host 'cd $dir && docker compose logs -f trader'"
