#!/usr/bin/env bash
# push.sh REPO VERSION — push REPO:latest, and REPO:VERSION only when the registry does not have it.
#
# DE tools pin :<VERSION>, so a published version tag is never moved: to publish a changed image,
# raise VERSION first (scripts/upstream.sh rebuild). Both tags must exist locally (make build).
set -euo pipefail

repo="${1:?usage: push.sh REPO VERSION}"
version="${2:?usage: push.sh REPO VERSION}"

docker push "$repo:latest"
if out=$(docker buildx imagetools inspect "$repo:$version" 2>&1); then
  echo "$repo:$version is already published and was left as it is; only :latest was pushed."
  echo "To publish a changed image under a version tag, raise VERSION (scripts/upstream.sh rebuild)."
elif grep -qiE 'not found|manifest unknown' <<<"$out"; then
  docker push "$repo:$version"
else
  # An outage or a failed login must not be read as "not published yet".
  echo "$out" >&2
  echo "push.sh: cannot tell whether $repo:$version is published; it was not pushed" >&2
  exit 1
fi
