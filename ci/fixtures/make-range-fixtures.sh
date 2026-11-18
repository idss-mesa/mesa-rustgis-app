#!/bin/sh
# make-range-fixtures.sh <dir> — add a Cloud-Optimized GeoTIFF and an indexed FlatGeobuf to the
# fixture user's home (<dir> as built by make-data-store.sh). RustGIS reads both in place by byte
# range, so the browser pass (docs/verification.md 1.3) uses them to see 206 responses from /fs.
#
# Needs GDAL's gdal_translate and ogr2ogr (3.1 or later). Without them on the host, run it in any
# GDAL image:
#   docker run --rm -i --user "$(id -u):$(id -g)" -v <dir>:/ds <gdal image> sh -s /ds < make-range-fixtures.sh
set -eu

dir="${1:?usage: make-range-fixtures.sh <dir>}"
home="$dir/iplant/home/rgistest"
[ -d "$home" ] || { echo "make-range-fixtures.sh: $home is missing; run make-data-store.sh first" >&2; exit 1; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# 1024 x 1024 cells over Arizona. The noise keeps the file near 2 MB after compression, so a
# reader that fetched it whole would stand out next to one that asks for tiles.
awk 'BEGIN {
  n = 1024; srand(7)
  print "ncols " n; print "nrows " n; print "xllcorner -115.0"; print "yllcorner 31.0"
  print "cellsize " 6.0 / n; print "NODATA_value -9999"
  for (r = 0; r < n; r++) {
    line = ""
    for (c = 0; c < n; c++)
      line = line (c ? " " : "") int(1200 + 900 * sin(r / 61.0) * cos(c / 47.0) + 400 * sin((r + c) / 13.0) + rand() * 60)
    print line
  }
}' > "$tmp/dem.asc"
gdal_translate -q -of COG -ot Int16 -a_srs EPSG:4326 -co COMPRESS=DEFLATE -co BLOCKSIZE=256 \
  "$tmp/dem.asc" "$home/dem-cog.tif"

awk 'BEGIN {
  srand(11); print "id,name,lon,lat,value"
  for (i = 1; i <= 20000; i++)
    printf "%d,site-%05d,%.5f,%.5f,%d\n", i, i, -115 + 6 * rand(), 31 + 6 * rand(), int(rand() * 1000)
}' > "$tmp/sites.csv"
rm -f "$home/sites.fgb"
# FlatGeobuf is written with a spatial index by default; without one RustGIS downloads it whole.
ogr2ogr -q -f FlatGeobuf "$home/sites.fgb" "$tmp/sites.csv" -nln sites -a_srs EPSG:4326 \
  -oo X_POSSIBLE_NAMES=lon -oo Y_POSSIBLE_NAMES=lat -oo KEEP_GEOM_COLUMNS=NO

ls -l "$home/dem-cog.tif" "$home/sites.fgb"
