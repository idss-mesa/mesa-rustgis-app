#!/bin/sh
# inject.sh <path to index.html> — add the overlay's stylesheet and script to the RustGIS page.
#
# The script tag is parser-blocking on purpose (no defer/async/module): it has to run before
# upstream's module script. Nothing else in the bundle changes; sw.js stays as shipped, because it
# fetches index.html from this server when it installs and never handles /host, /fs or /api.
set -eu

tag='<link rel="stylesheet" href="/host/rustgis-host.css" data-rgis-host><script src="/host/rustgis-host.js" data-rgis-host></script>'

file="${1:?usage: inject.sh <path to index.html>}"
[ -f "$file" ] || { echo "inject.sh: $file: not a file" >&2; exit 1; }

if grep -q 'data-rgis-host' "$file"; then
  exit 0
fi

count=$(awk '{ n += gsub(/<\/head>/, "&") } END { print n + 0 }' "$file")
if [ "$count" -ne 1 ]; then
  echo "inject.sh: $file has $count '</head>' tags, expected exactly 1; nothing changed" >&2
  exit 1
fi

tmp="$file.inject.$$"
trap 'rm -f "$tmp"' EXIT
awk -v tag="$tag" '
  { i = index($0, "</head>") }
  i { $0 = substr($0, 1, i - 1) tag substr($0, i) }
  { print }
' "$file" > "$tmp"
grep -q 'data-rgis-host></script></head>' "$tmp" || { echo "inject.sh: injection failed; nothing changed" >&2; exit 1; }
# Overwrite in place so the file keeps its owner and mode.
cat "$tmp" > "$file"
