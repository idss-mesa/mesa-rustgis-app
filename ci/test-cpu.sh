#!/usr/bin/env bash
# test-cpu.sh IMAGE — end-to-end check of the image against docs/architecture.md, run the way VICE
# runs it: uid 1000, read-only root filesystem, a volume at the working directory and a fixture
# Data Store at /data-store. Needs docker, curl, jq, openssl and sha256sum; no network.
#
# Prints one PASS/FAIL line per check and exits non-zero if any check failed.
set -uo pipefail

img="${1:?usage: test-cpu.sh IMAGE}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
for tool in docker curl jq openssl sha256sum; do
  command -v "$tool" >/dev/null || { echo "test-cpu.sh: $tool is required" >&2; exit 2; }
done

run="rgis-test-$$"
vol="$run-data"
rw="$run-rw"
ro="$run-ro"
bare="$run-bare"
tmp="$(mktemp -d)"
cleanup() {
  docker rm -f "$rw" "$ro" "$bare" >/dev/null 2>&1
  docker volume rm "$vol" >/dev/null 2>&1
  rm -rf "$tmp"
}
trap cleanup EXIT

results=()
fails=0
redirects=()
cors=()
status=""

# check NAME EXPECTED ACTUAL
check() {
  if [ "$2" = "$3" ]; then
    results+=("PASS  $1")
  else
    results+=("FAIL  $1: expected '$2', got '$3'")
    fails=$((fails + 1))
  fi
}

# req CURL_ARGS... URL — one request; sets $status, leaves the body in $tmp/body and the response
# headers in $tmp/hdr. Every response is also screened for redirects and CORS headers.
req() {
  : > "$tmp/body"
  : > "$tmp/hdr"
  status=$(curl -sS --path-as-is --max-time 120 -o "$tmp/body" -D "$tmp/hdr" -w '%{http_code}' "$@" 2>"$tmp/curl.err") ||
    status="curl-failed($(head -c 200 "$tmp/curl.err"))"
  case "$status" in
    301 | 302 | 303 | 307 | 308) redirects+=("$status ${*: -1}") ;;
  esac
  if grep -qi '^access-control-' "$tmp/hdr"; then cors+=("${*: -1}"); fi
}

# hdr NAME — value of a header of the last response ("" when absent).
hdr() { grep -i "^$1:" "$tmp/hdr" | tail -1 | sed 's/^[^:]*:[[:space:]]*//' | tr -d '\r'; }
# js FILTER — jq over the last body ("" when it is not JSON).
js() { jq -r "$1" "$tmp/body" 2>/dev/null; }

# expect NAME STATUS ERROR_CODE CURL_ARGS... — ERROR_CODE "" checks the status alone.
expect() {
  local name=$1 want=$2 code=$3
  shift 3
  req "$@"
  if [ -n "$code" ]; then
    check "$name" "$want $code" "$status $(js '.error.code // empty')"
  else
    check "$name" "$want" "$status"
  fi
}

# start NAME DOCKER_ARGS... — a container as VICE runs it: uid 1000, read-only root filesystem, a
# writable working directory. The short health interval lets the run see the health status.
start() {
  local name=$1
  shift
  docker run -d --name "$name" --user 1000 --read-only --health-interval=2s \
    --tmpfs /home/rustgis/data-store:uid=1000,gid=1000,mode=0755 \
    -p 127.0.0.1::8080 "$@" "$img" >/dev/null
}

# base_of NAME — wait for /healthz and print the container's base URL (the host port is assigned
# by docker and changes on restart).
base_of() {
  local port
  for _ in $(seq 60); do
    port=$(docker port "$1" 8080/tcp 2>/dev/null | head -1 | sed 's/.*://')
    if [ -n "$port" ] && [ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/healthz")" = 200 ]; then
      echo "http://127.0.0.1:$port"
      return 0
    fi
    sleep 0.5
  done
  return 1
}

fail_early() {
  echo "FAIL $img: $1" >&2
  docker logs "$rw" 2>&1 | tail -20 >&2
  exit 1
}

echo "-- fixture Data Store"
docker volume create "$vol" >/dev/null
docker run --rm -i --user 0 -v "$vol:/data-store" --entrypoint sh "$img" -s /data-store \
  < "$here/fixtures/make-data-store.sh" || fail_early "could not build the fixture"

echo "-- containers"
# The smallest allowed chunk limit, so the upload checks stay small.
vice=(-e IPLANT_USER=rgistest -e REDIRECT_URL=http://localhost:8080 -v "$vol:/data-store")
# The extra host stands in for an analysis host in the project checks.
start "$rw" "${vice[@]}" -e RGIS_MAX_CHUNK=262144 -e RGIS_ALLOWED_HOSTS=a0123abcd.cyverse.run ||
  fail_early "docker run failed"
start "$ro" "${vice[@]}" -e RGIS_READONLY=1 || fail_early "docker run failed (read-only)"
# No Data Store and no user: the app must still come up.
start "$bare" || fail_early "docker run failed (no Data Store)"
base=$(base_of "$rw") || fail_early "rgis-host did not answer /healthz"
robase=$(base_of "$ro") || fail_early "read-only rgis-host did not answer /healthz"
barebase=$(base_of "$bare") || fail_early "rgis-host without a Data Store did not answer /healthz"

home=/iplant/home/rgistest
mut=(-H 'X-RGIS-Host: 1')
json=("${mut[@]}" -H 'Content-Type: application/json')
put=(-X PUT "${mut[@]}" -H 'Content-Type: application/octet-stream')

echo "-- process"
check "runs as uid 1000" 1000 "$(docker exec "$rw" id -u 2>&1)"
docker exec "$rw" /opt/rustgis/bin/rgis-host healthcheck >/dev/null 2>&1
check "rgis-host healthcheck exits 0" 0 "$?"
check "entrypoint is tini -g" "/usr/bin/tini -g -- /opt/rustgis/bin/rgis-host" \
  "$(docker inspect -f '{{join .Config.Entrypoint " "}}' "$img")"

echo "-- static"
req "$base/healthz"
check "GET /healthz" "200 ok" "$status $(cat "$tmp/body")"
check "/healthz nosniff" nosniff "$(hdr X-Content-Type-Options)"
req "$base/"
check "GET /" 200 "$status"
check "/ content type" "text/html; charset=utf-8" "$(hdr Content-Type)"
check "/ cache-control" no-cache "$(hdr Cache-Control)"
check "/ frame-ancestors" "frame-ancestors 'self'" "$(hdr Content-Security-Policy)"
check "overlay tags injected once, before </head>" 1 "$(grep -c -F \
  '<link rel="stylesheet" href="/host/rustgis-host.css" data-rgis-host><script src="/host/rustgis-host.js" data-rgis-host></script></head>' "$tmp/body")"
check "no other data-rgis-host marker" 2 "$(grep -o 'data-rgis-host' "$tmp/body" | wc -l | tr -d ' ')"
wasm=$(grep -o 'rustgis-web-[0-9a-f]*_bg\.wasm' "$tmp/body" | head -1)
sri=$(sed -n 's/.*_bg\.wasm"[^>]*integrity="sha384-\([^"]*\)".*/\1/p' "$tmp/body" | head -1)
expect "GET /index.html" 200 "" "$base/index.html"
expect "GET /?data=... ignores the query" 200 "" "$base/?data=https%3A%2F%2Fexample.org%2Fa.tif&webgl"
req -H 'Accept-Encoding: gzip' "$base/"
check "/ gzip" "200 gzip" "$status $(hdr Content-Encoding)"
expect "GET /host/rustgis-host.js" 200 "" "$base/host/rustgis-host.js"
check "overlay js content type" "text/javascript; charset=utf-8" "$(hdr Content-Type)"
check "overlay js cache-control" no-cache "$(hdr Cache-Control)"
expect "GET /host/rustgis-host.css" 200 "" "$base/host/rustgis-host.css"
check "overlay css content type" "text/css; charset=utf-8" "$(hdr Content-Type)"
expect "GET /sw.js" 200 "" "$base/sw.js"
check "sw.js cache-control" no-cache "$(hdr Cache-Control)"
expect "GET /manifest.webmanifest" 200 "" "$base/manifest.webmanifest"
check "manifest content type" application/manifest+json "$(hdr Content-Type)"
check "bundle names a wasm file" yes "$([ -n "$wasm" ] && echo yes || echo no)"
req -I -H 'Accept-Encoding: gzip' "$base/$wasm"
check "wasm served compressed" "200 gzip" "$status $(hdr Content-Encoding)"
check "wasm content type" application/wasm "$(hdr Content-Type)"
check "wasm immutable" "public, max-age=31536000, immutable" "$(hdr Cache-Control)"
check "wasm Vary" Accept-Encoding "$(hdr Vary)"
req -I "$base/$wasm"
check "wasm identity when gzip is not accepted" "200 " "$status $(hdr Content-Encoding)"
if [ -n "$sri" ]; then
  check "decoded wasm matches the page's integrity hash" "$sri" \
    "$(curl -s --compressed "$base/$wasm" | openssl dgst -sha384 -binary | base64 | tr -d '\n')"
fi
expect "GET /index.html.gz" 404 "" "$base/index.html.gz"
expect "GET /.hidden (static)" 404 "" "$base/.hidden"
expect "GET /missing (no index fallback)" 404 "" "$base/missing"
expect "GET /host (directory)" 404 "" "$base/host"
expect "GET /snippets/ (directory)" 404 "" "$base/snippets/"
expect "POST /" 405 "" -X POST "$base/"
# Shapes that http.ServeMux or http.FileServer would redirect; only the no-3xx screen judges them.
for p in // /./index.html /../index.html /host/ /host/../index.html /index.html/ /fs/ /api /api/ /api/v1/list/; do
  req "$base$p"
done

echo "-- session"
req "$base/api/v1/session"
check "GET /api/v1/session" 200 "$status"
check "session content type" application/json "$(hdr Content-Type)"
check "session cache-control" no-store "$(hdr Cache-Control)"
check "session user" rgistest "$(js .user)"
check "session origin is the request host" "$base" "$(js .origin)"
check "session mode" launch "$(js .mode)"
check "session readOnly" false "$(js .readOnly)"
check "session root ids" home,shared,input,output,scratch "$(js '[.roots[].id] | join(",")')"
check "session root paths" "$home,/iplant/home/shared,/input,/output,/scratch" "$(js '[.roots[].path] | join(",")')"
check "session root writable" true,false,false,true,true "$(js '[.roots[].writable | tostring] | join(",")')"
check "session root exists" true,true,true,true,true "$(js '[.roots[].exists | tostring] | join(",")')"
check "session limits" "262144 0 268435456 16" "$(js '.limits | "\(.maxChunk) \(.maxUpload) \(.maxProject) \(.maxOpen)"')"
check "session versions" \
  "1 $(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$img" | sed -n 's/^RGIS_VERSION=//p') $(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$img" | sed -n 's/^RGIS_UPSTREAM=//p')" \
  "$(js '.versions | "\(.api) \(.host) \(.upstream)"')"

echo "-- list and stat"
req "$base/api/v1/list?path=$home"
check "list home" 200 "$status"
check "list order and hidden entries (dot-names, .irodssymlink, symlinks)" \
  "sub dir|cities.csv|notes.txt|points.geojson|stale.rgis|Zeta.txt" "$(js '[.entries[].name] | join("|")')"
check "list metadata" "$home home true " "$(js '"\(.path) \(.root) \(.writable) \(.nextCursor)"')"
check "list entry shape" "dir 0 file 37" \
  "$(js '[.entries[] | select(.name == "sub dir" or .name == "notes.txt") | "\(.type) \(.size)"] | join(" ")')"
check "list mtime is RFC 3339 UTC seconds" true \
  "$(js '.entries[0].mtime | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")')"
req "$base/api/v1/list?path=$home&limit=2"
cursor=$(js .nextCursor)
check "list page 1" "sub dir|cities.csv" "$(js '[.entries[].name] | join("|")')"
req "$base/api/v1/list?path=$home&limit=2&cursor=$cursor"
check "list page 2" "notes.txt|points.geojson" "$(js '[.entries[].name] | join("|")')"
expect "list limit=0" 400 bad_request "$base/api/v1/list?path=$home&limit=0"
expect "list on a file" 400 not_a_directory "$base/api/v1/list?path=$home/notes.txt"
expect "list /iplant/home (not a root)" 404 not_found "$base/api/v1/list?path=/iplant/home"
expect "list another user's home" 404 not_found "$base/api/v1/list?path=/iplant/home/otheruser"
expect "list /" 400 bad_path "$base/api/v1/list?path=/"
expect "list without path" 400 "" "$base/api/v1/list"
req "$base/api/v1/list?path=/iplant/home/shared"
check "list shared is not writable" "200 shared false" "$status $(js '"\(.root) \(.writable)"')"
req "$base/api/v1/stat?path=$home/notes.txt"
check "stat file" "200 notes.txt file 37 home true" "$status $(js '"\(.entry.name) \(.entry.type) \(.entry.size) \(.root) \(.writable)"')"
req "$base/api/v1/stat?path=$home"
check "stat root" "200 rgistest dir" "$status $(js '"\(.entry.name) \(.entry.type)"')"
expect "stat dotfile" 400 bad_path "$base/api/v1/stat?path=$home/.hidden"
expect "unknown /api route" 404 not_found "$base/api/v1/nope"
expect "POST /api/v1/session" 405 method_not_allowed -X POST "${json[@]}" -d '{}' "$base/api/v1/session"
check "405 carries Allow" yes "$([ -n "$(hdr Allow)" ] && echo yes || echo no)"

echo "-- /fs"
req "$base/fs$home/points.geojson"
check "GET /fs file" 200 "$status"
check "/fs content type" application/json "$(hdr Content-Type)"
check "/fs nosniff" nosniff "$(hdr X-Content-Type-Options)"
check "/fs attachment" attachment "$(hdr Content-Disposition)"
check "/fs CSP" "sandbox; default-src 'none'; frame-ancestors 'none'" "$(hdr Content-Security-Policy)"
check "/fs CORP" same-origin "$(hdr Cross-Origin-Resource-Policy)"
check "/fs cache-control" "private, no-cache" "$(hdr Cache-Control)"
check "/fs no Content-Encoding" "" "$(hdr Content-Encoding)"
etag=$(hdr ETag)
check "/fs strong ETag" yes "$([[ "$etag" =~ ^\"[0-9a-f]+-[0-9a-f]+\"$ ]] && echo yes || echo no)"
req -H "If-None-Match: $etag" "$base/fs$home/points.geojson"
check "conditional GET" 304 "$status"
req -I "$base/fs$home/notes.txt"
check "HEAD /fs" "200 bytes 37" "$status $(hdr Accept-Ranges) $(hdr Content-Length)"
check "/fs .txt content type" "text/plain; charset=utf-8" "$(hdr Content-Type)"
req -H 'Range: bytes=10-15' "$base/fs$home/notes.txt"
check "Range" "206 abcdef bytes 10-15/37" "$status $(cat "$tmp/body") $(hdr Content-Range)"
expect "Range past the end" 416 "" -H 'Range: bytes=100-' "$base/fs$home/notes.txt"
expect "path with an encoded space" 200 "" "$base/fs$home/sub%20dir/nested.geojson"
expect "csv from input root" 200 "" "$base/fs/input/input.csv"
check "/fs .csv content type" "text/csv; charset=utf-8" "$(hdr Content-Type)"
expect "shared root is readable" 200 "" "$base/fs/iplant/home/shared/example/points.geojson"
expect "symlink inside the root is not followed" 404 not_found "$base/fs$home/inroot-link.geojson"
expect "stat of a symlink" 404 not_found "$base/api/v1/stat?path=$home/inroot-link.geojson"
expect "symlink to a dot-file" 404 not_found "$base/fs$home/dotfile-link.txt"
expect "file through a symlink to a dot-directory" 404 not_found "$base/fs$home/dotdir-link/token"
expect "list through a symlink to a dot-directory" 404 not_found "$base/api/v1/list?path=$home/dotdir-link"
expect "directory on /fs" 404 not_found "$base/fs$home"
expect "directory with trailing slash" 400 bad_path "$base/fs$home/"
expect "GET /fs" 404 not_found "$base/fs"
expect "missing file" 404 not_found "$base/fs$home/nope.tif"
expect "traversal with .." 400 bad_path "$base/fs$home/../otheruser/secret.txt"
expect "traversal with %2e%2e" 400 bad_path "$base/fs$home/%2e%2e/otheruser/secret.txt"
expect "traversal with %2f" 400 bad_path "$base/fs$home/..%2fotheruser%2fsecret.txt"
expect "another user's home" 404 not_found "$base/fs/iplant/home/otheruser/secret.txt"
expect "symlink leaving the root" 404 not_found "$base/fs$home/escape-link"
expect "absolute symlink" 404 not_found "$base/fs$home/abs-link"
expect "dotfile" 400 bad_path "$base/fs$home/.hidden"
expect ".irodssymlink" 400 bad_path "$base/fs$home/link.irodssymlink"
expect "backslash" 400 bad_path "$base/fs$home/a%5cb"
expect "NUL byte" 400 bad_path "$base/fs$home/a%00b"
expect "double slash" 400 bad_path "$base/fs$home//notes.txt"
expect "POST /fs" 405 method_not_allowed -X POST "$base/fs$home/notes.txt"

echo "-- guard"
expect "Sec-Fetch-Site: same-origin" 200 "" -H 'Sec-Fetch-Site: same-origin' "$base/fs$home/notes.txt"
expect "Sec-Fetch-Site: none" 200 "" -H 'Sec-Fetch-Site: none' "$base/fs$home/notes.txt"
expect "Sec-Fetch-Site: same-site GET /fs" 403 forbidden_origin -H 'Sec-Fetch-Site: same-site' "$base/fs$home/notes.txt"
expect "Sec-Fetch-Site: cross-site GET /fs" 403 forbidden_origin -H 'Sec-Fetch-Site: cross-site' "$base/fs$home/notes.txt"
expect "Sec-Fetch-Site: same-site GET /api" 403 forbidden_origin -H 'Sec-Fetch-Site: same-site' "$base/api/v1/session"
expect "Sec-Fetch-Site: same-site GET list" 403 forbidden_origin -H 'Sec-Fetch-Site: same-site' "$base/api/v1/list?path=$home"
expect "foreign Origin GET /fs" 403 forbidden_origin -H 'Origin: https://a0000ffff.cyverse.run' "$base/fs$home/notes.txt"
expect "foreign Origin GET /api" 403 forbidden_origin -H 'Origin: https://a0000ffff.cyverse.run' "$base/api/v1/session"
expect "Origin: null" 403 forbidden_origin -H 'Origin: null' "$base/fs$home/notes.txt"
expect "Origin equal to Host" 200 "" -H "Origin: $base" "$base/fs$home/notes.txt"
expect "Origin equal to the REDIRECT_URL host" 200 "" -H 'Origin: http://localhost:8080' "$base/fs$home/notes.txt"
expect "Origin with the other scheme" 403 forbidden_origin -H "Origin: https://${base#http://}" "$base/fs$home/notes.txt"
expect "Host equal to the REDIRECT_URL host" 200 "" -H 'Host: localhost:8080' "$base/fs$home/notes.txt"
# DNS rebinding: the page is same-origin to the browser; only its Host is foreign.
rebind=(-H 'Host: evil.example:8080' -H 'Origin: http://evil.example:8080' -H 'Sec-Fetch-Site: same-origin')
expect "foreign Host with matching Origin GET /fs" 403 forbidden_origin "${rebind[@]}" "$base/fs$home/notes.txt"
expect "foreign Host with matching Origin GET /api" 403 forbidden_origin "${rebind[@]}" "$base/api/v1/session"
expect "foreign Host with matching Origin mkdir" 403 forbidden_origin \
  -X POST "${json[@]}" "${rebind[@]}" -d "{\"path\":\"$home/rebound\"}" "$base/api/v1/mkdir"
expect "foreign Host: nothing was created" 404 not_found "$base/api/v1/stat?path=$home/rebound"
expect "foreign Host alone" 403 forbidden_origin -H 'Host: evil.example' "$base/api/v1/session"
expect "static is served under any Host" 200 "" -H 'Host: evil.example' "$base/"
expect "static is not guarded" 200 "" -H 'Sec-Fetch-Site: cross-site' "$base/"
expect "mutation without X-RGIS-Host" 403 missing_header \
  -X POST -H 'Content-Type: application/json' -d "{\"path\":\"$home/nohdr\"}" "$base/api/v1/mkdir"
expect "upload without X-RGIS-Host" 403 missing_header \
  -X PUT -H 'Content-Type: application/octet-stream' --data-binary x \
  "$base/api/v1/upload?path=$home/nohdr.bin&id=00000000000000000000000000000001&offset=0"
expect "mkdir sent as a form" 415 bad_content_type \
  -X POST "${mut[@]}" -H 'Content-Type: application/x-www-form-urlencoded' -d "path=$home/form" "$base/api/v1/mkdir"
expect "upload sent as text/plain" 415 bad_content_type \
  -X PUT "${mut[@]}" -H 'Content-Type: text/plain' --data-binary x \
  "$base/api/v1/upload?path=$home/text.bin&id=00000000000000000000000000000001&offset=0"
expect "mutation with a foreign Origin" 403 forbidden_origin \
  -X POST "${json[@]}" -H 'Origin: https://a0000ffff.cyverse.run' -d "{\"path\":\"$home/evil\"}" "$base/api/v1/mkdir"
expect "OPTIONS /api" 405 method_not_allowed -X OPTIONS "$base/api/v1/upload"
check "OPTIONS carries Allow" yes "$([ -n "$(hdr Allow)" ] && echo yes || echo no)"
expect "OPTIONS /fs" 405 method_not_allowed -X OPTIONS "$base/fs$home/notes.txt"
req -X OPTIONS -H "Origin: $base" -H 'Access-Control-Request-Method: PUT' \
  -H 'Access-Control-Request-Headers: x-rgis-host' "$base/api/v1/upload"
check "preflight is not approved" 405 "$status"

echo "-- mkdir"
req -X POST "${json[@]}" -d "{\"path\":\"$home/new folder\"}" "$base/api/v1/mkdir"
check "mkdir" "201 $home/new folder new folder dir 0" "$status $(js '"\(.path) \(.entry.name) \(.entry.type) \(.entry.size)"')"
expect "mkdir existing" 409 exists -X POST "${json[@]}" -d "{\"path\":\"$home/new folder\"}" "$base/api/v1/mkdir"
expect "mkdir on a root" 409 exists -X POST "${json[@]}" -d "{\"path\":\"$home\"}" "$base/api/v1/mkdir"
expect "mkdir without parent" 404 not_found -X POST "${json[@]}" -d "{\"path\":\"$home/no/such\"}" "$base/api/v1/mkdir"
expect "mkdir dot name" 400 bad_path -X POST "${json[@]}" -d "{\"path\":\"$home/.secret\"}" "$base/api/v1/mkdir"
expect "mkdir bad body" 400 bad_request -X POST "${json[@]}" -d 'not json' "$base/api/v1/mkdir"
expect "mkdir in shared" 403 read_only -X POST "${json[@]}" -d '{"path":"/iplant/home/shared/x"}' "$base/api/v1/mkdir"
expect "mkdir in input" 403 read_only -X POST "${json[@]}" -d '{"path":"/input/x"}' "$base/api/v1/mkdir"
expect "mkdir in output" 201 "" -X POST "${json[@]}" -d '{"path":"/output/run1"}' "$base/api/v1/mkdir"
expect "mkdir in scratch" 201 "" -X POST "${json[@]}" -d '{"path":"/scratch/tmp1"}' "$base/api/v1/mkdir"
expect "mkdir where the filesystem refuses" 403 forbidden -X POST "${json[@]}" -d '{"path":"/output/locked/x"}' "$base/api/v1/mkdir"

echo "-- upload"
head -c 700000 /dev/urandom > "$tmp/up.bin"
head -c 262144 "$tmp/up.bin" > "$tmp/c1"
tail -c +262145 "$tmp/up.bin" | head -c 262144 > "$tmp/c2"
tail -c +524289 "$tmp/up.bin" > "$tmp/c3"
uid=$(openssl rand -hex 16)
q="path=$home/upload%20test.bin&id=$uid"
req -I "$base/api/v1/upload?$q"
check "upload HEAD before" "200 0" "$status $(hdr Upload-Offset)"
req "${put[@]}" --data-binary "@$tmp/c1" "$base/api/v1/upload?$q&offset=0"
check "upload chunk 1" "200 262144 false" "$status $(js '"\(.offset) \(.committed)"')"
req "$base/api/v1/list?path=$home"
check "temp file is not listed" 0 "$(js '[.entries[].name | select(startswith(".rgis-upload-") or . == "upload test.bin")] | length')"
expect "temp file is not readable" 400 bad_path "$base/fs$home/.rgis-upload-$uid.part"

docker restart -t 10 "$rw" >/dev/null
base=$(base_of "$rw") || fail_early "rgis-host did not come back after a restart"
req -I "$base/api/v1/upload?$q"
check "upload offset survives a server restart" "200 262144" "$status $(hdr Upload-Offset)"
req "${put[@]}" --data-binary "@$tmp/c3" "$base/api/v1/upload?$q&offset=0"
check "upload wrong offset" "409 offset_mismatch 262144" "$status $(js '"\(.error.code) \(.offset)"')"
req "${put[@]}" --data-binary "@$tmp/up.bin" "$base/api/v1/upload?$q&offset=262144"
check "upload oversized chunk" "413 chunk_too_large 262144" "$status $(js '"\(.error.code) \(.offset)"')"
req "${put[@]}" --data-binary "@$tmp/c2" "$base/api/v1/upload?$q&offset=262144"
check "upload chunk 2" "200 524288 false" "$status $(js '"\(.offset) \(.committed)"')"
req "${put[@]}" --data-binary "@$tmp/c3" "$base/api/v1/upload?$q&offset=524288&commit=1&size=1"
check "upload commit with wrong size" "409 size_mismatch 700000" "$status $(js '"\(.error.code) \(.offset)"')"
req "${put[@]}" --data-binary '' "$base/api/v1/upload?$q&offset=700000&commit=1&size=700000"
check "upload commit" "201 700000 true $home/upload test.bin file 700000" \
  "$status $(js '"\(.offset) \(.committed) \(.path) \(.entry.type) \(.entry.size)"')"
check "upload round-trips by sha256" "$(sha256sum < "$tmp/up.bin")" "$(curl -s "$base/fs$home/upload%20test.bin" | sha256sum)"
req -I "$base/api/v1/upload?$q"
check "upload HEAD after commit" "200 0" "$status $(hdr Upload-Offset)"

uid2=$(openssl rand -hex 16)
q2="path=$home/upload%20test.bin&id=$uid2"
req "${put[@]}" --data-binary hello "$base/api/v1/upload?$q2&offset=0&commit=1&size=5"
check "upload over an existing file" "409 exists" "$status $(js .error.code)"
req "${put[@]}" --data-binary hello "$base/api/v1/upload?$q2&offset=0&commit=1&size=5&overwrite=1"
check "upload with overwrite=1" "201 true 5" "$status $(js '"\(.committed) \(.entry.size)"')"
check "overwritten content" hello "$(curl -s "$base/fs$home/upload%20test.bin")"
req "${put[@]}" --data-binary '' "$base/api/v1/upload?path=$home/empty.bin&id=$uid2&offset=0&commit=1&size=0"
check "upload empty file" "201 true 0" "$status $(js '"\(.committed) \(.entry.size)"')"

uid3=$(openssl rand -hex 16)
q3="path=/output/aborted.bin&id=$uid3"
expect "upload chunk to output" 200 "" "${put[@]}" --data-binary "@$tmp/c1" "$base/api/v1/upload?$q3&offset=0"
expect "upload abort" 204 "" -X DELETE "${mut[@]}" "$base/api/v1/upload?$q3"
req -I "$base/api/v1/upload?$q3"
check "upload offset after abort" "200 0" "$status $(hdr Upload-Offset)"
expect "upload abort when nothing is there" 204 "" -X DELETE "${mut[@]}" "$base/api/v1/upload?$q3"
expect "upload abort without X-RGIS-Host" 403 missing_header -X DELETE "$base/api/v1/upload?$q3"
expect "upload bad id" 400 bad_request "${put[@]}" --data-binary x "$base/api/v1/upload?path=$home/x.bin&id=XYZ&offset=0"
expect "upload bad commit value" 400 bad_request "${put[@]}" --data-binary x "$base/api/v1/upload?path=$home/x.bin&id=$uid3&offset=0&commit=yes"
expect "upload commit without size" 400 bad_request "${put[@]}" --data-binary x "$base/api/v1/upload?path=$home/x.bin&id=$uid3&offset=0&commit=1"
expect "upload without parent" 404 not_found "${put[@]}" --data-binary x "$base/api/v1/upload?path=$home/no/such.bin&id=$uid3&offset=0"
expect "upload onto a directory" 409 exists "${put[@]}" --data-binary x "$base/api/v1/upload?path=$home/sub%20dir&id=$uid3&offset=0"
expect "upload to shared" 403 read_only "${put[@]}" --data-binary x "$base/api/v1/upload?path=/iplant/home/shared/x.bin&id=$uid3&offset=0"
expect "upload to input" 403 read_only "${put[@]}" --data-binary x "$base/api/v1/upload?path=/input/x.bin&id=$uid3&offset=0"
expect "upload where the filesystem refuses" 403 forbidden "${put[@]}" --data-binary x "$base/api/v1/upload?path=/output/locked/x.bin&id=$uid3&offset=0"
expect "upload dot name" 400 bad_path "${put[@]}" --data-binary x "$base/api/v1/upload?path=$home/.x&id=$uid3&offset=0"
expect "upload traversal" 400 bad_path "${put[@]}" --data-binary x "$base/api/v1/upload?path=$home/../otheruser/x&id=$uid3&offset=0"
expect "upload through a symlink to a dot-directory" 404 not_found "${put[@]}" --data-binary x \
  "$base/api/v1/upload?path=$home/dotdir-link/authorized_keys&id=$uid3&offset=0&commit=1&size=1"
expect "mkdir through a symlink to a dot-directory" 404 not_found \
  -X POST "${json[@]}" -d "{\"path\":\"$home/dotdir-link/x\"}" "$base/api/v1/mkdir"
check "the dot-directory is untouched" token "$(docker exec "$rw" ls -A "/data-store$home/.secrets" 2>&1 | tr '\n' ' ' | sed 's/ $//')"

echo "-- project"
req "$base/api/v1/project$home/stale.rgis"
check "GET project" 200 "$status"
check "project content type" application/json "$(hdr Content-Type)"
check "project attachment, no ETag" "attachment " "$(hdr Content-Disposition) $(hdr ETag)"
check "project Content-Length" "$(wc -c < "$tmp/body" | tr -d ' ')" "$(hdr Content-Length)"
check "placeholder and localhost origins rewritten" 2 "$(grep -c -F "\"$base/fs/" "$tmp/body")"
check "placeholder is gone" 0 "$(grep -c -F 'rgis-host.invalid' "$tmp/body")"
check "other hosts are untouched" 2 "$(grep -c -e 'https://a1b2c3d4e.cyverse.run/fs/iplant' -e 'https://example.org/fs/keep.tif' "$tmp/body")"
cp "$tmp/body" "$tmp/project"
check "rest of the project is byte-identical" same "$(curl -s "$base/fs$home/stale.rgis" |
  sed -e "s#https://rgis-host.invalid/fs/#$base/fs/#g" -e "s#http://localhost:9999/fs/#$base/fs/#g" |
  cmp -s - "$tmp/project" && echo same || echo differs)"
req -H 'Host: a0123abcd.cyverse.run' "$base/api/v1/project$home/stale.rgis"
check "analysis host: stale analysis origin and placeholder rewritten" "200 2" \
  "$status $(grep -c -F '"http://a0123abcd.cyverse.run/fs/' "$tmp/body")"
check "analysis host: localhost and foreign hosts untouched" 2 \
  "$(grep -c -e 'http://localhost:9999/fs/input' -e 'https://example.org/fs/keep.tif' "$tmp/body")"
req -I "$base/api/v1/project$home/stale.rgis"
check "HEAD project: no length, the file is not read" "200 application/json " "$status $(hdr Content-Type) $(hdr Content-Length)"
expect "project route on a non-.rgis file" 400 bad_request "$base/api/v1/project$home/points.geojson"
expect "project missing" 404 not_found "$base/api/v1/project$home/nope.rgis"
expect "project is guarded" 403 forbidden_origin -H 'Sec-Fetch-Site: same-site' "$base/api/v1/project$home/stale.rgis"

echo "-- RGIS_READONLY=1"
req "$robase/api/v1/session"
check "read-only session" "200 true false,false,false,false,false" \
  "$status $(js '"\(.readOnly) \([.roots[].writable | tostring] | join(","))"')"
expect "read-only: reads still work" 200 "" "$robase/fs$home/notes.txt"
expect "read-only: mkdir" 403 read_only -X POST "${json[@]}" -d "{\"path\":\"$home/ro\"}" "$robase/api/v1/mkdir"
expect "read-only: mkdir in scratch" 403 read_only -X POST "${json[@]}" -d '{"path":"/scratch/ro"}' "$robase/api/v1/mkdir"
expect "read-only: upload" 403 read_only "${put[@]}" --data-binary x \
  "$robase/api/v1/upload?path=$home/ro.bin&id=$uid3&offset=0&commit=1&size=1"
expect "read-only: upload abort" 403 read_only -X DELETE "${mut[@]}" "$robase/api/v1/upload?path=$home/ro.bin&id=$uid3"
expect "read-only: nothing was written" 404 not_found "$robase/api/v1/stat?path=$home/ro.bin"

echo "-- no Data Store, no IPLANT_USER"
expect "bare: GET /" 200 "" "$barebase/"
req "$barebase/api/v1/session"
check "bare: session without user or home root" "200 [] shared,input,output,scratch" \
  "$status [$(js .user)] $(js '[.roots[].id] | join(",")')"
check "bare: Data Store roots do not exist, scratch does" false,false,false,true "$(js '[.roots[].exists | tostring] | join(",")')"
expect "bare: list under a missing root" 404 not_found "$barebase/api/v1/list?path=/input"
expect "bare: without a public URL only loopback hosts are served" 403 forbidden_origin \
  -H 'Host: a0123abcd.cyverse.run' "$barebase/api/v1/session"

echo "-- health"
for _ in $(seq 30); do
  health=$(docker inspect -f '{{.State.Health.Status}}' "$rw" 2>&1)
  [ "$health" = starting ] || break
  sleep 1
done
check "docker health status" healthy "$health"

echo "-- every response"
check "no 301/302/303/307/308 on any request" "" "${redirects[*]:-}"
check "no Access-Control-* header on any response" "" "${cors[*]:-}"

echo
printf '%s\n' "${results[@]}"
echo
total=${#results[@]}
if [ "$fails" -ne 0 ]; then
  echo "-- rgis-host log (last 30 lines)"
  docker logs "$rw" 2>&1 | tail -30
  echo "FAIL $img: $fails of $total checks failed"
  exit 1
fi
echo "PASS $img: $total checks"
