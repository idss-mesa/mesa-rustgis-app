# MESA RustGIS — the RustGIS web app (opengeos/RustGIS) with a CyVerse Data Store panel, for VICE.
#
# One process (rgis-host) serves the upstream bundle, the overlay and the file API on port 8080.
# The build context is latest/; the pins live in ../VERSION and ../UPSTREAM_REF and arrive as build
# args, so build with `make build` (see ../Makefile) rather than a bare `docker build`.

# zip: the unpatched upstream release bundle. src (M4): a patched source build.
ARG RUSTGIS_SOURCE=zip

FROM ubuntu:25.10@sha256:7cc5e35f6567ee8c66d2abb4aab0fd866669e6207c237c3a8f0947a5c7f17092 AS base

# ---- web-zip: the upstream release bundle, checksum-verified, with the overlay tags injected ----
FROM base AS web-zip
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl unzip && \
    rm -rf /var/lib/apt/lists/*
ARG RUSTGIS_TAG
ARG RUSTGIS_WEB_ZIP_SHA256
RUN : "${RUSTGIS_TAG:?build arg RUSTGIS_TAG is required (see UPSTREAM_REF; use make build)}" && \
    : "${RUSTGIS_WEB_ZIP_SHA256:?build arg RUSTGIS_WEB_ZIP_SHA256 is required (see UPSTREAM_REF; use make build)}" && \
    curl -fsSL --retry 3 -o /tmp/web.zip \
      "https://github.com/opengeos/RustGIS/releases/download/${RUSTGIS_TAG}/RustGIS-${RUSTGIS_TAG#v}-web.zip" && \
    echo "${RUSTGIS_WEB_ZIP_SHA256}  /tmp/web.zip" > /tmp/web.zip.sha256 && \
    sha256sum -c /tmp/web.zip.sha256 && \
    mkdir /web && \
    unzip -q /tmp/web.zip -d /web && \
    rm /tmp/web.zip /tmp/web.zip.sha256 && \
    chmod -R u=rwX,go=rX /web
COPY inject.sh /tmp/inject.sh
RUN sh /tmp/inject.sh /web/index.html

# ---- web-src: M4 STUB, NOT BUILT. ----
# M4 replaces this stage with upstream's own build stage at RUSTGIS_COMMIT (the Rust image digest
# that tag's Dockerfile pins), `git am --3way patches/*`, a Trunk build with `--features
# host-bridge`, inject.sh and `cargo xtask web-pwa`, leaving the bundle in /web; the `web` stage
# below then becomes `FROM web-${RUSTGIS_SOURCE} AS web` and the runtime sets RGIS_MODE=bridge.
FROM base AS web-src
RUN echo "RUSTGIS_SOURCE=src is not implemented until M4" >&2; exit 1

# ---- web: the bundle plus the overlay, precompressed ----
FROM web-zip AS web
ARG RUSTGIS_SOURCE
RUN [ "${RUSTGIS_SOURCE}" = zip ] || { echo "RUSTGIS_SOURCE=${RUSTGIS_SOURCE} is not implemented until M4" >&2; exit 1; }
COPY host/ /web/host/
# rgis-host serves a .gz sibling when the browser accepts gzip; -k keeps the original for the rest.
RUN find /web -type f \( -name '*.wasm' -o -name '*.js' -o -name '*.css' -o -name '*.html' \
      -o -name '*.txt' -o -name '*.webmanifest' \) -exec gzip -9 -k {} + && \
    chmod -R u=rwX,go=rX /web

# ---- host: the rgis-host binary (Go standard library only, so the build needs no network) ----
FROM golang:1.27.3@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e AS host
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOPROXY=off GOFLAGS=-buildvcs=false
WORKDIR /src
COPY server/ ./
RUN go vet ./... && go test ./...
RUN go build -trimpath -ldflags="-s -w" -o /out/rgis-host .

# ---- runtime ----
FROM base AS runtime

ARG VERSION=dev
ARG RUSTGIS_TAG
ARG RUSTGIS_COMMIT=unknown

LABEL org.label-schema.name="CyVerse VICE MESA RustGIS" \
      org.label-schema.description="RustGIS web app with a CyVerse Data Store panel for CyVerse VICE" \
      org.label-schema.url="https://cyverse.org" \
      org.label-schema.vcs-url="https://github.com/idss-mesa/mesa-rustgis-app" \
      org.label-schema.vendor="CyVerse" \
      org.label-schema.version="${VERSION}" \
      org.label-schema.schema-version="1.0.0" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.cyverse.mesa.rustgis.upstream-tag="${RUSTGIS_TAG}" \
      org.cyverse.mesa.rustgis.upstream-commit="${RUSTGIS_COMMIT}"

# ubuntu:24.04 ships an `ubuntu` user at uid 1000; VICE runs the tool as 1000, so that id is ours.
RUN apt-get update && \
    apt-get install -y --no-install-recommends tini && \
    rm -rf /var/lib/apt/lists/* && \
    if id ubuntu >/dev/null 2>&1; then userdel --remove ubuntu; fi && \
    groupadd --gid 1000 rustgis && \
    useradd --uid 1000 --gid 1000 --create-home --home-dir /home/rustgis --shell /usr/sbin/nologin rustgis && \
    install -d -o 1000 -g 1000 -m 0755 /home/rustgis/data-store

COPY --from=host /out/rgis-host /opt/rustgis/bin/rgis-host
COPY --from=web /web/ /opt/rustgis/app/

ENV HOME=/home/rustgis \
    RGIS_VERSION=${VERSION} \
    RGIS_UPSTREAM=${RUSTGIS_TAG} \
    RGIS_MODE=launch

USER 1000:1000
# The DE mounts a per-analysis volume here; session scratch lives in it.
WORKDIR /home/rustgis/data-store

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/opt/rustgis/bin/rgis-host", "healthcheck"]

ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/opt/rustgis/bin/rgis-host"]
