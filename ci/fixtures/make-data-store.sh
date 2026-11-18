#!/bin/sh
# make-data-store.sh <dir> — build a small fake CyVerse Data Store mount for tests and local runs.
#
# Layout (user rgistest), as the DE's CSI driver presents it under /data-store:
#   <dir>/iplant/home/rgistest   the user's home (read-write root)
#   <dir>/iplant/home/shared     community data (read-only root)
#   <dir>/iplant/home/otheruser  another user's home: must never be reachable
#   <dir>/input, <dir>/output    analysis inputs and outputs
#   <dir>/output/locked          mode 0555: the filesystem itself refuses writes there
#
# POSIX sh, so it also runs inside the image:
#   docker run --rm -i --user 0 -v <volume>:/data-store --entrypoint sh <image> -s /data-store < make-data-store.sh
# Run as root it hands the tree to FIXTURE_OWNER (default 1000:1000, the image user).
set -eu

dir="${1:?usage: make-data-store.sh <dir>}"
home="$dir/iplant/home/rgistest"
shared="$dir/iplant/home/shared"

mkdir -p "$home/sub dir" "$shared/example" "$dir/iplant/home/otheruser" "$dir/input" "$dir/output"

geojson() {
  cat > "$1" <<'EOF'
{"type":"FeatureCollection","features":[
{"type":"Feature","properties":{"name":"Tucson"},"geometry":{"type":"Point","coordinates":[-110.9747,32.2226]}},
{"type":"Feature","properties":{"name":"Phoenix"},"geometry":{"type":"Point","coordinates":[-112.074,33.4484]}},
{"type":"Feature","properties":{"name":"Flagstaff"},"geometry":{"type":"Point","coordinates":[-111.6513,35.1983]}}
]}
EOF
}

geojson "$home/points.geojson"
geojson "$home/sub dir/nested.geojson"
geojson "$shared/example/points.geojson"

cat > "$home/cities.csv" <<'EOF'
name,longitude,latitude,population
Tucson,-110.9747,32.2226,542629
Phoenix,-112.0740,33.4484,1608139
Flagstaff,-111.6513,35.1983,76831
EOF
cp "$home/cities.csv" "$dir/input/input.csv"

# Exactly 37 bytes: the Range tests address it by offset.
printf '0123456789abcdefghijklmnopqrstuvwxyz\n' > "$home/notes.txt"
# Sorts last only when names are compared case-insensitively.
printf 'zeta\n' > "$home/Zeta.txt"

# Not a complete RustGIS project: only the URL prefixes matter to the project route's byte-level
# rewrite. One URL of each kind: placeholder, another analysis's origin, localhost, a foreign host.
cat > "$home/stale.rgis" <<'EOF'
{"layers":[
{"url":"https://rgis-host.invalid/fs/iplant/home/rgistest/points.geojson"},
{"url":"https://a1b2c3d4e.cyverse.run/fs/iplant/home/rgistest/cities.csv"},
{"url":"http://localhost:9999/fs/input/input.csv"},
{"url":"https://example.org/fs/keep.tif"}
]}
EOF

printf 'community data\n' > "$shared/readme.txt"
printf 'not yours\n' > "$dir/iplant/home/otheruser/secret.txt"

# Writable root, unwritable directory: the service passes the write on and the filesystem says no.
mkdir -p "$dir/output/locked"
chmod 555 "$dir/output/locked"

# Entries the service must hide or refuse. No symlink is followed: one that stays inside the root
# could otherwise lead to a dot-name, which the path rules keep out of reach.
printf 'hidden\n' > "$home/.hidden"
mkdir -p "$home/.secrets"
printf 'token\n' > "$home/.secrets/token"
printf 'irods symlink marker\n' > "$home/link.irodssymlink"
ln -sfn ../otheruser/secret.txt "$home/escape-link"
ln -sfn /etc/passwd "$home/abs-link"
ln -sfn points.geojson "$home/inroot-link.geojson"
ln -sfn .hidden "$home/dotfile-link.txt"
ln -sfn .secrets "$home/dotdir-link"

if [ "$(id -u)" -eq 0 ]; then
  chown -R -h "${FIXTURE_OWNER:-1000:1000}" "$dir"
fi
