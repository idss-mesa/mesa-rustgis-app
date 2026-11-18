# Tracking upstream RustGIS

[opengeos/RustGIS](https://github.com/opengeos/RustGIS) releases about weekly and its `main`
branch moves faster than that. This repository follows **release tags only**, one at a time, and
never builds from `main`.

What exists today (M1, M3): the image ships the unmodified release web zip. What is planned (M4):
a source build with a small patch series. Sections 1 to 3 describe the present; section 4 is a
plan and nothing in it is implemented.

## 1. What is pinned, and where

| File | Content | Now |
|---|---|---|
| `UPSTREAM_REF` | `RUSTGIS_TAG`, `RUSTGIS_COMMIT`, `RUSTGIS_WEB_ZIP_SHA256`: shell-sourceable `KEY=value` lines, passed to the build as build args of the same names | `v0.6.0`, `6db08fe7203a54d568deeefcc21f6a7dcc38f26a`, `302bbf07…d152` |
| `VERSION` | `<upstream version>-<n>`. `n` starts at 1 for each upstream version and counts this repository's releases on it | `0.6.0-1` |

- The zip is `https://github.com/opengeos/RustGIS/releases/download/<tag>/RustGIS-<version>-web.zip`.
  The build downloads it and fails unless its SHA-256 equals `RUSTGIS_WEB_ZIP_SHA256`.
- `RUSTGIS_COMMIT` is not used by the zip build. It records which source the zip corresponds to,
  so that source references in `architecture.md` can be checked, and it is what the M4 source
  build will check out.
- `VERSION` is the image tag and the DE tool version. Change `n` for any change to the image that
  is not an upstream bump (server, overlay, base image digest): `scripts/upstream.sh rebuild`.
- The same value is written in `de/tool.json` (`version`, `container.image.tag`) and in
  `de/app.json` and `de/app-readonly.json` (`version`, `tools[0].version`). `pr.yml` fails when
  they differ from `VERSION`; `upstream.sh bump` and `rebuild` change all of them together.
- The base images in `latest/Dockerfile` (`ubuntu`, `golang`) are pinned by digest. A rebuild
  therefore reproduces the same base; fixes to it arrive as dependabot pull requests
  (`.github/dependabot.yml`). Treat each as a rebuild: run `scripts/upstream.sh rebuild` on the
  pull request's branch before merging, so that the changed image gets a new version tag (the
  published one is never moved, and `security.yml` scans the tag in `VERSION`). A `golang` pull
  request that changes the version, not only the digest, also needs the `golang:1.27.3` quoted
  in `README.md`, `docs/architecture.md` and `docs/verification.md` updated; the `Makefile`
  reads the image from the Dockerfile.
- The runtime reports both: `GET /api/v1/session` → `versions.host`, `versions.upstream`.

## 2. What this repository depends on upstream for

A new upstream release can break the image without failing the build. These are the behaviours
relied on, with where they were read at v0.6.0. Re-check each on a bump.

| Dependency | Read from (v0.6.0) | Used by | How to re-check |
|---|---|---|---|
| The release has a `RustGIS-<version>-web.zip` asset, a flat archive with `index.html` at the top | release assets | Dockerfile | `unzip -l` |
| `index.html` contains `</head>` exactly once | bundle | `inject.sh` (exits 1 otherwise) | the build |
| Hashed asset names match `-[0-9a-f]{8,}(_bg)?\.(js\|wasm)$`; everything else must not be cached long | `packaging/docker/nginx.conf` | static cache rules | diff upstream `nginx.conf` between the tags |
| No COOP/COEP or other special response headers are required | `packaging/docker/nginx.conf` | static headers | same diff |
| `?data=<url>`: split on `&`, decoded once, `+` literal, at most 16, absolute `http(s)` only, ignored when `project=` is present | `apps/rustgis-web/src/web.rs` (~line 278) | overlay URL builders, the 16 cap | read the function; browser pass B8 to B11 |
| A project is opened only by `?project=`; the same address as `?data=` is parsed as a data file and fails | `web.rs:128`, `200` | "Open in new map" on a project file uses the project URL | browser pass B23 |
| A Shapefile is given by its `.shp` address; the `.dbf`, `.shx`, `.prj`, `.cpg` are fetched from beside it, and a part given on its own is refused | `crates/data/src/io.rs` (`shapefile_part_urls`, ~line 235) | overlay leaves selected parts out of the launch URL | browser pass B24 |
| **Quirk:** the WebGL backend is chosen when the raw query string *contains* `webgl` anywhere, a substring test (`query().contains("webgl")`), not a parameter lookup | `web.rs:114` | overlay writes `webgl` inside an address as `%77ebgl` | read `start()`; browser pass B25. If upstream makes it a parameter lookup, `queryValue` can go |
| `?project=<url>`: one plain `GET`; project named from the last URL segment, not decoded; a raw space in the address is refused | `lib.rs` (~3133, ~5377) | project route, overlay URL builders | browser pass B19 |
| A saved `.rgis` keeps URLs only for layers read by range; layers downloaded whole are stored in the project | a project saved at v0.6.0 | project route (what there is to rewrite) | browser pass B19 |
| `webgl`, `gpu`, `lang` query parameters | `web.rs` | carried over by the overlay | read |
| Data requests must not be redirected | `web.rs` fetch code | no-redirect rule | read; rule holds regardless |
| A `window` capture-phase `keydown` listener that calls `preventDefault` on Ctrl/⌘ shortcuts | `web.rs:1277` | overlay key handling | browser pass B3 to B5 |
| eframe's event targets: keys, `pointerdown`, wheel on the canvas; clipboard and pointer release on `document` | eframe 0.37.0 `crates/eframe/src/web/events.rs` | overlay focus and event rules | check the eframe version in upstream `Cargo.lock`; browser pass B3 to B7 |
| Element ids `rustgis_canvas`, `rustgis_loading`, `rustgis_update`, `rfd-overlay` | `index.html`, rfd | overlay (focuses the canvas, avoids the others) | grep the new bundle |
| `localStorage` keys start with `rustgis.` | source | overlay stays out of them | grep |
| `sw.js` caches only the app shell | `sw.js` in the bundle | `/fs`, `/api`, `/host` must reach the network | read the new `sw.js` |
| Event `TrunkApplicationStarted` | Trunk | bridge detection (inert until M4) | grep `index.html` |
| Format handling: which formats are range-read, the 256 MiB download cap | upstream docs, `crates/tiles/src/cache.rs` | README "Limits", `RGIS_MAX_PROJECT` default | upstream changelog and docs |
| Bundle ships `LICENSE` and `THIRD-PARTY-LICENSES.txt` | bundle | `NOTICE` | `unzip -l` |

## 3. Bump procedure (zip build)

`scripts/upstream.sh bump [TAG]` does steps 1, 3 and 4: it pins the given release (the newest
one without an argument), checks that the zip has an `index.html` with exactly one `</head>`,
and rewrites `UPSTREAM_REF`, `VERSION` and the version fields in `de/*.json`. It then lists the
documents that still quote the old upstream version (`README.md`, `de/app-docs.md`, `docs/`),
which step 6 updates by hand. The other steps are by hand. `checkout`, `apply` and `refresh`
print `not implemented until M4` and exit 2.

1. **Choose the tag.** A published release, not a pre-release, not `main`:

   ```bash
   gh release list --repo opengeos/RustGIS --limit 5
   ```

2. **Read the changelog** between the old and new tags for anything touching the web app's
   start-up parameters, remote fetching, the file dialogs, `index.html`, the service worker, or
   `packaging/docker/`.

3. **Collect the three values.**

   ```bash
   TAG=v0.7.0; VER=${TAG#v}
   git ls-remote https://github.com/opengeos/RustGIS "refs/tags/$TAG" "refs/tags/$TAG^{}"
   curl -fsSLO "https://github.com/opengeos/RustGIS/releases/download/$TAG/RustGIS-$VER-web.zip"
   sha256sum "RustGIS-$VER-web.zip"
   ```

   Use the peeled (`^{}`) commit if the tag is annotated, the plain one otherwise. If upstream
   publishes checksums for the release, compare.

4. **Edit** `UPSTREAM_REF` (all three lines), `VERSION` (`<VER>-1`), and the same version in
   `de/tool.json` (`version`, `container.image.tag`), `de/app.json` and `de/app-readonly.json`
   (`version`, `tools[0].version`). The lint job in `pr.yml` fails if any of them differs.

5. **Inspect the new bundle** against the table in section 2: `unzip -l`, the `</head>` count,
   `sw.js`, the element ids. Diff `packaging/docker/nginx.conf` and the web app's `web.rs` between
   the two tags:

   ```bash
   gh api "repos/opengeos/RustGIS/compare/v0.6.0...$TAG" --jq '.files[].filename' \
     | grep -E 'apps/rustgis-web/|packaging/docker/|Cargo.lock'
   ```

6. **Update the documents that quote upstream**: line references and behaviour notes in
   `architecture.md` (sections 5 and 6, and U8/U9), the dependency table above, the "Limits"
   section of `README.md`, and every place that names the release or the image version (the
   list `bump` printed: `README.md`, `de/app-docs.md`, `architecture.md` section 8).
   `architecture.md` is the contract: change it before the code.

7. **Build and verify**: the image build (which runs the server tests), `ci/test-cpu.sh`, then the
   browser pass in [`verification.md`](verification.md) section 1.3. The browser pass is not
   optional on a bump: the overlay's keyboard handling and the launch URLs depend on upstream
   internals that no automated test here covers.

8. **Release**: merge to `main`; CI pushes `:latest`, and `:<VERSION>` if Harbor answers "not
   found" for that tag (a published version tag is never moved; `make push` follows the same
   rule, and both fail if Harbor cannot be asked). In the DE add a new tool
   version and a new app version labelled with the image tag. Do not edit the existing versions:
   tools used by public apps are protected, and old share links name a version.

If a release breaks something that cannot be fixed quickly, stay on the previous pin. There is no
requirement to follow every release.

Rebuilding without an upstream change (base image update, a server fix): leave `UPSTREAM_REF`
alone and run `scripts/upstream.sh rebuild`. It increases `n` in `VERSION` and in the version
fields of `de/*.json`, and lists the documents that still quote the old version. Then step 7
(the browser pass only where the overlay or the bundle changed) and step 8.

## 4. Planned: the M4 patch series

Not implemented. `patches/`, the source-build stage and `.github/workflows/upstream-bump.yml` do
not exist yet. This section records the intended workflow so that the seams left in M1 and M3
make sense. Details will change when the work is done.

### 4.1 Goal

Make RustGIS's own Open and Save dialogs use the Data Store, and let the panel add layers to the
current map without a reload ("bridge mode"). This needs a few hooks in the web app that upstream
does not have. The hooks will be proposed upstream; the patch series exists for as long as they
are not merged.

### 4.2 Shape of the series

Against the pinned tag (never `main`; the two differ), touching `apps/rustgis-web` only, behind a
cargo feature `host-bridge` so that the patched source builds to the same binary when the feature
is off. Estimated at 150 to 250 lines:

| Patch | Content |
|---|---|
| `0001` | New `src/host.rs`: JavaScript externs for `window.rustgisHost.pick` / `.save`, and a command queue |
| `0002` | Hook in `open_async` (v0.6.0 `web.rs`, about line 1129): call `pick` when it is a function, else upstream's picker |
| `0003` | Hook in `download()` (about line 1223): call `save` when it is a function, else upstream's download |
| `0004` | Drain the queue in `WebShell::logic`; export `rustgis_host_run(command, paramsJson)` |

The names `rustgisHost.pick`, `rustgisHost.save` and `rustgis_host_run` are already reserved in
`architecture.md` section 7. The overlay must leave `pick` and `save` undefined until they work,
because the patched app falls back to upstream behaviour only when they are not functions.

### 4.3 Seams already in place

| Seam | State now |
|---|---|
| `ARG RUSTGIS_SOURCE=zip` in the Dockerfile | Any other value fails the build with `RUSTGIS_SOURCE=<value> is not implemented until M4` |
| Overlay detects `window.rustgis_host_run` (or `window.wasmBindings.rustgis_host_run`) | Sets `mode = "bridge"` and the root class; no other effect |
| `RGIS_MODE` | `launch`; the Dockerfile will set `bridge` only for a source build |
| Placeholder origin `https://rgis-host.invalid/fs/` | The project route already rewrites it; nothing writes it yet |
| `scripts/upstream.sh checkout\|apply\|refresh` | Stubs exit 2 with `not implemented until M4` (`bump` and `rebuild` work for the zip build) |

### 4.4 Intended workflow

- **`scripts/upstream.sh checkout`**: clone upstream at `RUSTGIS_COMMIT` into a work directory
  outside the build context.
- **`scripts/upstream.sh apply`**: `git am --3way patches/*.patch` on that checkout. A failure
  stops here, loudly; nothing is built from a partly patched tree.
- **`scripts/upstream.sh refresh`**: after editing commits in the checkout, regenerate
  `patches/` with `git format-patch` from the pinned commit, so the series in this repository
  is always exactly what applies to the pin.
- **`scripts/upstream.sh bump <tag>`**: update `UPSTREAM_REF` and `VERSION`, check out the new
  tag, try `apply`, and report.
- **Source build** (`RUSTGIS_SOURCE=src`): a Dockerfile stage that reproduces upstream's own build
  stage at the pinned commit, with the Rust toolchain image digest that tag's Dockerfile uses,
  applies the series, builds with Trunk and `--features host-bridge`, runs `inject.sh`, then
  upstream's `cargo xtask web-pwa` so the service worker's file list includes the overlay. The
  cold build time is unmeasured; if it is long, the web stage will be published as its own image
  keyed by upstream commit and patch hash.
- **Fallback**: `RUSTGIS_SOURCE=zip` stays supported. When the series does not apply to a new
  release, the choice is to ship that release in launch mode from the zip, or to stay on the old
  pin in bridge mode, until the series is rebased.
- **`upstream-bump.yml`** (weekly): apply the series to the newest release tag and run
  `cargo check` for `wasm32-unknown-unknown`. On success open a pull request with the new pin; on
  failure open an issue naming the patch that failed. It never merges and never pushes an image.

### 4.5 Rebasing the series

When a new tag moves the code under a patch:

1. `checkout` the new tag, `apply`; resolve the conflicts in the checkout, one commit per patch.
2. Keep each patch minimal and feature-gated; do not fold unrelated fixes into the series.
3. `refresh`, then build with `RUSTGIS_SOURCE=src` and run the whole of `verification.md`.
4. If upstream has merged a hook, drop that patch and use upstream's name for it; update
   `architecture.md` section 7 first.

The series should shrink over time. If it grows, or a release needs more than a mechanical
rebase, that is the signal to stay on the zip build and take the question upstream.
