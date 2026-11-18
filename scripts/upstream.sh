#!/usr/bin/env bash
# upstream.sh — manage the pin on opengeos/RustGIS (UPSTREAM_REF) and the image version (VERSION).
#
#   upstream.sh bump [TAG]   pin a release (default: the newest), from its web zip
#   upstream.sh rebuild      raise n in VERSION: a changed image on the same upstream release
#   upstream.sh checkout     M4
#   upstream.sh apply        M4
#   upstream.sh refresh      M4
#
# bump changes UPSTREAM_REF; bump and rebuild change VERSION and, with it, the version fields in
# de/*.json (pr.yml requires them to agree). The rest of a bump (reading the changelog, checking
# the new bundle, the browser pass) is in docs/upstream-updates.md.
set -euo pipefail

upstream=opengeos/RustGIS
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

die() {
  echo "upstream.sh: $*" >&2
  exit 1
}

# api PATH — GET from the GitHub API; GITHUB_TOKEN, when set, lifts the anonymous rate limit.
api() {
  curl -fsSL ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} \
    -H 'Accept: application/vnd.github+json' "https://api.github.com/repos/$upstream/$1"
}

# next_n — VERSION's n plus one.
next_n() {
  local n
  n=$(sed -n 's/^.*-\([0-9][0-9]*\)$/\1/p' "$root/VERSION")
  [ -n "$n" ] || die "VERSION '$(cat "$root/VERSION")' is not <upstream version>-<n>"
  echo $((n + 1))
}

# set_version NEW — write VERSION and replace the old value in the DE definitions, which carry it
# as the tool version, the image tag and the app version.
set_version() {
  local new=$1 old f
  old=$(cat "$root/VERSION")
  echo "$new" > "$root/VERSION"
  for f in "$root"/de/*.json; do
    sed "s/\"${old//./\\.}\"/\"$new\"/g" "$f" > "$f.tmp" && mv "$f.tmp" "$f"
    grep -qF "\"$new\"" "$f" || die "${f#"$root"/} did not name version $old; set its version fields to $new by hand"
  done
  echo "VERSION $new (was $old); de/*.json follow"
}

# mentions TEXT — list the documents that still quote TEXT; prose is updated by hand.
mentions() {
  local files
  files=$(
    cd "$root" || exit
    grep -rlF -- "$1" README.md de/app-docs.md docs || true
  )
  [ -z "$files" ] || printf 'still quoting %s, update by hand:\n%s\n' "$1" "$files"
}

bump() {
  local tag=${1:-} ver commit zip sha heads old_tag old_sha n
  for tool in curl git sha256sum unzip; do
    command -v "$tool" >/dev/null || die "$tool is required"
  done
  if [ -z "$tag" ]; then
    # releases/latest never names a draft or a pre-release.
    tag=$(api releases/latest | sed -n 's/^ *"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
  fi
  [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "'$tag' is not a release tag (vX.Y.Z)"
  ver=${tag#v}

  # An annotated tag lists its commit on the peeled (^{}) line, which sorts last.
  commit=$(git ls-remote "https://github.com/$upstream" "refs/tags/$tag" "refs/tags/$tag^{}" | tail -1 | cut -f1)
  [[ "$commit" =~ ^[0-9a-f]{40}$ ]] || die "tag $tag not found upstream"

  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  zip="$tmp/web.zip"
  curl -fsSL --retry 3 -o "$zip" "https://github.com/$upstream/releases/download/$tag/RustGIS-$ver-web.zip" ||
    die "release $tag has no RustGIS-$ver-web.zip"
  sha=$(sha256sum "$zip" | cut -d' ' -f1)
  # What the Dockerfile and inject.sh need from the bundle. awk counts without failing on zero.
  unzip -l "$zip" index.html >/dev/null 2>&1 || die "the $tag zip has no index.html at its top level"
  heads=$(unzip -p "$zip" index.html | awk '{ n += gsub(/<\/head>/, "&") } END { print n + 0 }')
  [ "$heads" -eq 1 ] || die "index.html in the $tag zip has $heads '</head>' tags, expected 1"

  # shellcheck source=/dev/null
  old_tag=$(. "$root/UPSTREAM_REF" && echo "$RUSTGIS_TAG")
  # shellcheck source=/dev/null
  old_sha=$(. "$root/UPSTREAM_REF" && echo "$RUSTGIS_WEB_ZIP_SHA256")
  if [ "$old_tag:$old_sha" = "$tag:$sha" ]; then
    echo "already at $tag"
    return 0
  fi
  # Same tag, new zip: upstream replaced the asset. Count it as a rebuild of ours.
  n=1
  if [ "$old_tag" = "$tag" ]; then n=$(next_n); fi
  printf 'RUSTGIS_TAG=%s\nRUSTGIS_COMMIT=%s\nRUSTGIS_WEB_ZIP_SHA256=%s\n' "$tag" "$commit" "$sha" > "$root/UPSTREAM_REF"
  echo "pinned $tag ($commit)"
  set_version "$ver-$n"
  if [ "$old_tag" != "$tag" ]; then mentions "${old_tag#v}"; fi
  echo "next: docs/upstream-updates.md section 3, from step 5"
}

rebuild() {
  local old n
  old=$(cat "$root/VERSION")
  n=$(next_n)
  set_version "${old%-*}-$n"
  mentions "$old"
}

case "${1:-}" in
  bump)
    shift
    bump "$@"
    ;;
  rebuild)
    rebuild
    ;;
  checkout | apply | refresh)
    echo "not implemented until M4" >&2
    exit 2
    ;;
  *)
    echo "usage: upstream.sh bump [TAG] | rebuild | checkout | apply | refresh" >&2
    exit 2
    ;;
esac
