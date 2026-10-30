#!/bin/sh
# Runs on the build server (started over SSH by ci/ssh-build.sh): builds the
# image and pushes it to every Harbor given. The build server must have docker
# and already be logged in to those Harbors.
#
#   remote-build.sh <context dir> <image name> <image tag> <harbor/project>...
#
# BUILDER_IMAGE and BASE_IMAGE, when set, override the Dockerfile's FROM
# images; GOPROXY, when set, overrides its Go module proxy.
set -eu

context=$1 name=$2 tag=$3
shift 3

local_image="$name:$tag"
# Image references have no spaces, so the args can be split on whitespace.
build_args=""
if [ -n "${BUILDER_IMAGE:-}" ]; then
  build_args="$build_args --build-arg BUILDER_IMAGE=$BUILDER_IMAGE"
fi
if [ -n "${BASE_IMAGE:-}" ]; then
  build_args="$build_args --build-arg BASE_IMAGE=$BASE_IMAGE"
fi
if [ -n "${GOPROXY:-}" ]; then
  build_args="$build_args --build-arg GOPROXY=$GOPROXY"
fi
# shellcheck disable=SC2086
docker build $build_args -t "$local_image" "$context"
trap 'docker rmi "$local_image" >/dev/null 2>&1 || true' EXIT

for repo in "$@"; do
  echo "push: $repo/$local_image"
  docker tag "$local_image" "$repo/$local_image"
  docker push "$repo/$local_image"
  docker rmi "$repo/$local_image" >/dev/null
done
