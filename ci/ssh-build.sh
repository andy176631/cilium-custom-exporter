#!/bin/sh
# Runs in the job container (CI_JOB_IMAGE, needs ssh and tar): copies the
# component's source to the build server over SSH and runs ci/remote-build.sh
# there.
#
#   ssh-build.sh <component dir> <image tag>
#
# CI/CD variables:
#   BUILD_SERVER_HOST         build server IP or hostname (required)
#   BUILD_SERVER_SSH_KEY      SSH private key, File type preferred (required)
#   BUILD_SERVER_USER         SSH user (default root)
#   BUILD_SERVER_PORT         SSH port (default 22)
#   BUILD_SERVER_KNOWN_HOSTS  known_hosts line(s) for the build server
#                             (optional; without it the host key is accepted
#                             on first connect)
#
# BUILDER_IMAGE and BASE_IMAGE (from ci/ci-config.yml) are passed on to
# override the Dockerfile's FROM images; empty keeps the Dockerfile default.
set -eu

component=$1 tag=$2
: "${BUILD_SERVER_HOST:?set CI/CD variable BUILD_SERVER_HOST}"
: "${BUILD_SERVER_SSH_KEY:?set CI/CD variable BUILD_SERVER_SSH_KEY}"
user=${BUILD_SERVER_USER:-root}
port=${BUILD_SERVER_PORT:-22}
list=${HARBOR_LIST_FILE:-ci/harbor-registries.txt}

repos=""
while read -r repo _ || [ -n "$repo" ]; do
  case "$repo" in '' | '#'*) continue ;; esac
  repos="$repos $repo"
done < "$list"
if [ -z "$repos" ]; then
  echo "no Harbor listed in $list" >&2
  exit 1
fi

# A variable of type File holds a path; a Variable holds the content itself.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if [ -f "$BUILD_SERVER_SSH_KEY" ]; then
  cat "$BUILD_SERVER_SSH_KEY" > "$tmp/key"
else
  printf '%s\n' "$BUILD_SERVER_SSH_KEY" > "$tmp/key"
fi
chmod 600 "$tmp/key"

known_hosts_opt="-o StrictHostKeyChecking=accept-new"
if [ -n "${BUILD_SERVER_KNOWN_HOSTS:-}" ]; then
  if [ -f "$BUILD_SERVER_KNOWN_HOSTS" ]; then
    cat "$BUILD_SERVER_KNOWN_HOSTS" > "$tmp/known_hosts"
  else
    printf '%s\n' "$BUILD_SERVER_KNOWN_HOSTS" > "$tmp/known_hosts"
  fi
  known_hosts_opt="-o StrictHostKeyChecking=yes"
fi

remote() {
  # shellcheck disable=SC2086
  ssh -i "$tmp/key" -p "$port" -o BatchMode=yes -o IdentitiesOnly=yes \
    -o UserKnownHostsFile="$tmp/known_hosts" $known_hosts_opt \
    "$user@$BUILD_SERVER_HOST" "$@"
}

workdir="/tmp/gitlab-build/${CI_PROJECT_PATH_SLUG:-cilium-custom-exporter}-${CI_JOB_ID:-$$}"
echo "build on $user@$BUILD_SERVER_HOST:$workdir"
trap 'remote rm -rf "$workdir" || true; rm -rf "$tmp"' EXIT

tar -cf - "$component" ci/remote-build.sh |
  remote "mkdir -p '$workdir' && tar -x -C '$workdir'"

# Tag and repos are safe to pass unquoted: the tag is checked by the
# workflow rule and the Harbor list has no spaces per entry.
remote BUILDER_IMAGE="'${BUILDER_IMAGE:-}'" BASE_IMAGE="'${BASE_IMAGE:-}'" \
  sh "$workdir/ci/remote-build.sh" "$workdir/$component" "$component" "$tag" $repos
