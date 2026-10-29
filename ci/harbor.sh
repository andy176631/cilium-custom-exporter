#!/bin/sh
# Reads HARBOR_REGISTRIES (see ci/release-config.yml) for the release jobs.
# Plain POSIX sh: it runs in the busybox shell of the Kaniko debug image.
#
#   harbor.sh refs <component> <version>  print every image reference to push
#   harbor.sh auth <file>                 write a docker config.json with the
#                                         credentials of every registry
set -eu

# Prints "<registry/project> <credential prefix>" for each non-comment line.
registries() {
  if [ -z "${HARBOR_REGISTRIES:-}" ]; then
    echo "HARBOR_REGISTRIES is empty; set it in ci/release-config.yml" >&2
    exit 1
  fi
  printf '%s\n' "$HARBOR_REGISTRIES" | while read -r repo prefix _; do
    case "$repo" in '' | '#'*) continue ;; esac
    if [ -z "${prefix:-}" ]; then
      echo "HARBOR_REGISTRIES: '$repo' has no credential prefix" >&2
      exit 1
    fi
    echo "$repo $prefix"
  done
}

refs() {
  component=$1 version=$2
  latest=false
  if [ "${PUSH_LATEST:-true}" = "true" ]; then
    case "$version" in *-*) ;; *) latest=true ;; esac
  fi
  list=$(registries)
  while read -r repo _; do
    echo "$repo/$component:$version"
    if [ "$latest" = true ]; then
      echo "$repo/$component:latest"
    fi
  done <<EOF
$list
EOF
}

auth() {
  out=$1
  entries=""
  list=$(registries)
  while read -r repo prefix; do
    host=${repo%%/*}
    eval "user=\${${prefix}_USER:-}"
    eval "password=\${${prefix}_PASSWORD:-}"
    if [ -z "$user" ] || [ -z "$password" ]; then
      echo "Missing CI/CD variables ${prefix}_USER / ${prefix}_PASSWORD for $host" >&2
      exit 1
    fi
    token=$(printf '%s:%s' "$user" "$password" | base64 | tr -d '\n')
    entries="${entries:+$entries,}\"$host\":{\"auth\":\"$token\"}"
  done <<EOF
$list
EOF
  printf '{"auths":{%s}}\n' "$entries" > "$out"
}

case "${1:-}" in
  refs) refs "$2" "$3" ;;
  auth) auth "$2" ;;
  *) echo "usage: $0 refs <component> <version> | auth <file>" >&2; exit 2 ;;
esac
