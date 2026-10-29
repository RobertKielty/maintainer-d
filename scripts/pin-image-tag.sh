#!/usr/bin/env bash
# Rewrites the newTag of one or more images in deploy/manifests/kustomization.yaml.
# Does not touch the cluster; `make manifests-apply` deploys the result.
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
KUSTOMIZATION="${KUSTOMIZATION:-${ROOT_DIR}/deploy/manifests/kustomization.yaml}"

usage() {
  echo "usage: $0 <tag> <image-repo> [<image-repo>...]" >&2
  echo "example: $0 main-94cc6c1 ghcr.io/cncf/maintainerd-web" >&2
  exit 2
}

[[ $# -ge 2 ]] || usage
tag="$1"
shift

# Same rule as the OCI distribution spec for tags.
if [[ ! "${tag}" =~ ^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$ ]]; then
  echo "invalid image tag: ${tag}" >&2
  exit 2
fi

if [[ ! -f "${KUSTOMIZATION}" ]]; then
  echo "missing kustomization: ${KUSTOMIZATION}" >&2
  exit 2
fi

tmp=$(mktemp "${KUSTOMIZATION}.XXXXXX")
trap 'rm -f "${tmp}"' EXIT

# Each images entry is a `- name:` line followed by its `newTag:` line.
# awk exits 1 if any requested repo has no entry, so a typo never deploys.
awk -v tag="${tag}" -v repos="$*" '
  BEGIN { n = split(repos, list, " "); for (i = 1; i <= n; i++) want[list[i]] = 1 }
  /^  - name: / { current = $3; print; next }
  /^    newTag: / && (current in want) {
    sub(/newTag: .*/, "newTag: " tag)
    done[current] = 1
    current = ""
    print
    next
  }
  { print }
  END {
    for (r in want) if (!(r in done)) { print "no images entry for " r > "/dev/stderr"; missing = 1 }
    exit missing
  }
' "${KUSTOMIZATION}" > "${tmp}"

# Copy rather than mv so the file keeps its permissions (mktemp makes 0600).
cat "${tmp}" > "${KUSTOMIZATION}"
for repo in "$@"; do
  echo "pinned ${repo}:${tag}"
done
