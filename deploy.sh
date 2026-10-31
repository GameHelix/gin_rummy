#!/usr/bin/env bash
# Deploys the trader to a server prepared by candylario/infra: builds the pushed
# commit for the server's CPU as trader:COMMIT, copies it over SSH with
# compose.yaml and its settings, and restarts the stack on it. Only a clean, pushed tree
# ships, so the server never runs code that is not in the repository. The
# last few images stay on the server, so a rollback is one command and no
# build. Its database stays on the server.
#
#   ./deploy.sh deploy@SERVER             # deploy the commit checked out here
#   ./deploy.sh --rollback deploy@SERVER  # run the image before the current one
# shellcheck disable=SC2029 # $dir is meant to expand here, before ssh.
set -euo pipefail
name=trader
dir=/srv/$name
keep=3 # images kept on the server, the running one included
cd "$(dirname "$0")"

rollback=
if [ "${1:-}" = --rollback ]; then
  rollback=1
  shift
fi
host=${1:?usage: ./deploy.sh [--rollback] deploy@SERVER}

# current prints the image compose runs on the server, from its settings.
current() { ssh "$host" "sed -n 's/^IMAGE=//p' $dir/.env 2>/dev/null"; }
# images lists the service's images on the server, newest first.
images() { ssh "$host" "docker images $name --format '{{.Repository}}:{{.Tag}}'" | grep -v '<none>'; }
# switch makes compose run an image already on the server.
switch() { ssh "$host" "cd $dir && sed -i '/^IMAGE=/d' .env && echo IMAGE=$1 >>.env && docker compose up -d --no-build --remove-orphans"; }

if [ -n "$rollback" ]; then
  now=$(current)
  previous=$(images | awk -v cur="$now" 'found { print; exit } $0 == cur { found = 1 }')
  if [ -z "$previous" ]; then
    echo "no image older than ${now:-the current one} on $host to roll back to" >&2
    exit 1
  fi
  switch "$previous"
  echo "Rolled $host back from $now to $previous. Logs: ssh $host 'cd $dir && docker compose logs -f $name'"
  exit 0
fi

if [ -n "$(git status --porcelain)" ]; then
  echo "the working tree has changes that are not committed; commit or stash them first" >&2
  exit 1
fi
git fetch -q
commit=$(git rev-parse --short=12 HEAD)
if ! git merge-base --is-ancestor HEAD '@{upstream}'; then
  echo "commit $commit is not pushed; git push first" >&2
  exit 1
fi
image=$name:$commit

# The settings come from the one file next to the repos when infra is there.
if [ -x ../infra/env.sh ]; then
  settings=$(../infra/env.sh $name)
elif [ -f .env ]; then
  settings=$(cat .env)
else
  echo "no settings: neither ../infra/env.sh nor .env" >&2
  exit 1
fi

case $(ssh "$host" uname -m) in
x86_64) platform=linux/amd64 ;;
aarch64 | arm64) platform=linux/arm64 ;;
*)
  echo "unsupported server CPU" >&2
  exit 1
  ;;
esac

docker buildx build --platform "$platform" --build-arg "COMMIT=$commit" --tag "$image" --load .
ssh "$host" "mkdir -p $dir"
docker save "$image" | gzip | ssh "$host" 'gunzip | docker load --quiet'
scp -q compose.yaml "$host:$dir/compose.yaml"
ssh "$host" "umask 077 && cat >$dir/.env" <<<"$settings"
ssh "$host" "docker network inspect candylario >/dev/null 2>&1 || docker network create candylario >/dev/null"
switch "$image"
# Keep the last few images for a rollback; the rest go, as do dangling layers.
images | tail -n +$((keep + 1)) | ssh "$host" 'xargs -r docker rmi >/dev/null 2>&1; docker image prune -f >/dev/null'
echo "Deployed $image to $host. Logs: ssh $host 'cd $dir && docker compose logs -f $name'"
