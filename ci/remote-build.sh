#!/bin/sh
# Runs on the build server (started over SSH by ci/ssh-build.sh): builds the
# image and pushes it to every Harbor given. The build server must have docker
# and already be logged in to those Harbors.
#
#   remote-build.sh <context dir> <image name> <image tag> <harbor/project>...
set -eu

context=$1 name=$2 tag=$3
shift 3

local_image="$name:$tag"
docker build -t "$local_image" "$context"
trap 'docker rmi "$local_image" >/dev/null 2>&1 || true' EXIT

for repo in "$@"; do
  echo "push: $repo/$local_image"
  docker tag "$local_image" "$repo/$local_image"
  docker push "$repo/$local_image"
  docker rmi "$repo/$local_image" >/dev/null
done
