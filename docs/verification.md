# Verification

Two checklists: what can be checked on a workstation (part 1), and what can only be measured in
the Discovery Environment (part 2, milestone M2). Part 2 has not been run. Until it has, every
statement about the DE in this repository is an assumption; they are numbered U1 to U12 in
[`architecture.md`](architecture.md) section 9.

## 1. Local

### 1.1 Server tests (automated)

```bash
docker run --rm -v "$PWD/latest/server:/src" -w /src golang:1.27.3 sh -c 'go vet ./... && go test ./...'
```

The host's Go is not used. The tests need no network, use only temporary directories, and must
pass both as root (the command above) and as uid 1000 (add `--user 1000 -e HOME=/tmp -e GOCACHE=/tmp/gocache`).
`make test-server` runs both, in the golang image the Dockerfile names. The image build runs
the same two commands, so a failing test fails the build.

### 1.2 Image smoke test (automated)

```bash
ci/test-cpu.sh harbor.cyverse.org/vice/mesa-rustgis:latest
```

It starts the image as the DE does (`--user 1000`, `--read-only` with a writable working
directory, `IPLANT_USER=rgistest`, `REDIRECT_URL` set) over the fixture Data Store from
`ci/fixtures/make-data-store.sh`, and checks it with HTTP requests. The script covers every row
of this table (some with other values: a 37-byte file for the ranges, a restart of the same
container for L23):

| # | Check | Expected |
|---|---|---|
| L1 | `GET /` and `GET /healthz` | 200; `/` contains `data-rgis-host` exactly as injected |
| L2 | `GET` the `.wasm` with `Accept-Encoding: gzip` | 200, `Content-Encoding: gzip`, `Content-Type: application/wasm`, `Vary: Accept-Encoding`, immutable `Cache-Control` |
| L3 | `GET /index.html`, `/host/rustgis-host.js` | 200, `Cache-Control: no-cache` |
| L4 | `GET /api/v1/session` | 200; user `rgistest`; five roots in order; `mode` `launch` |
| L5 | `GET /fs/<file>` with `Range: bytes=0-99` | 206, `Content-Range`, 100 bytes |
| L6 | `GET /fs/<file>` with a range past the end | 416 |
| L7 | `HEAD /fs/<file>` | 200, `Accept-Ranges: bytes`, `Content-Length`, `ETag`, `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff` |
| L8 | `GET /fs/<file>` with `If-None-Match: <etag>` | 304 |
| L9 | `GET /fs/<directory>`, `/fs`, `/fs/`, a path with `//`, a path with a trailing slash | 404 or 400, **never a 3xx** |
| L10 | Traversal: `/fs/iplant/home/rgistest/../../../etc/passwd`, sent without client-side normalisation (`curl --path-as-is`), and the `%2e%2e` form | 400 `bad_path` |
| L11 | Symlinks in the fixture: out of the root (absolute and relative), inside the root, to a dot-file, and a path through one to a dot-directory (`/fs`, `stat`, `list`, upload, `mkdir`) | 404 for every one; none is listed; nothing is written into the dot-directory |
| L12 | Dot-file: `/fs/iplant/home/rgistest/.secret`; and it is absent from `list` | 400 `bad_path`; not listed |
| L13 | Another user's home: `/fs/iplant/home/someoneelse/x` | 404 |
| L14 | `GET /fs/<file>` with `Sec-Fetch-Site: same-site`; with `cross-site` | 403 `forbidden_origin` |
| L15 | `GET /api/v1/list?...` with `Origin: https://evil.example`; with `Origin: null` | 403 `forbidden_origin` |
| L16 | `POST /api/v1/mkdir` without `X-RGIS-Host` | 403 `missing_header` |
| L17 | `POST /api/v1/mkdir` with `Content-Type: text/plain`; `PUT` upload with a form type | 415 `bad_content_type` |
| L18 | `mkdir` and upload under `/iplant/home/shared` and `/input` | 403 `read_only` |
| L19 | `OPTIONS` on `/fs/...` and `/api/v1/upload` | 405; no `Access-Control-*` header in any response of the whole run |
| L20 | Chunked upload of a multi-chunk file: `HEAD`, several `PUT`s, `commit=1` | 200 … 201; sha256 of the stored file equals the source |
| L21 | Upload with a wrong `offset` | 409 `offset_mismatch` with the true `offset` |
| L22 | Upload to an existing name without, then with, `overwrite=1` | 409 `exists`; then 201 |
| L23 | Upload half a file, stop the container, start a new one on the same Data Store, `HEAD`, finish | `Upload-Offset` equals the bytes sent; final sha256 matches |
| L24 | `DELETE /api/v1/upload` | 204; the `.part` file is gone; nothing else is |
| L25 | A `.rgis` holding `https://rgis-host.invalid/fs/…` and `http://localhost:9999/fs/…`, fetched through `/api/v1/project/…` | Both prefixes replaced by the current origin; rest of the file byte-identical |
| L26 | Second container with `RGIS_READONLY=1`: `mkdir`, `PUT`, `DELETE` | 403 `read_only`; session reports `readOnly: true` and no writable root |
| L27 | Container with no `/data-store` mounted and no `IPLANT_USER` | `/` 200; session 200 with `user` `""`, no `home` root, Data Store roots `exists: false`; no crash |
| L28 | `docker inspect` health status after start | `healthy` |
| L29 | Every response collected during the run | No status 301, 302, 303, 307 or 308 |
| L30 | `mkdir` and upload into a mode-0555 directory under a writable root | 403 `forbidden` |
| L31 | DNS rebinding shape: `Host: evil.example:8080` with `Origin: http://evil.example:8080` and `Sec-Fetch-Site: same-origin`, on `/fs`, `/api/v1/session` and `mkdir`; the same `Host` alone; any `Host` but loopback on the container without a public URL | 403 `forbidden_origin`; nothing created; `/` still 200 |
| L32 | `Origin` with the right host and the other scheme (`https` against the `http` public URL) | 403 `forbidden_origin` |
| L33 | `Host` equal to the `REDIRECT_URL` host; `Host` listed in `RGIS_ALLOWED_HOSTS` (the project checks) | 200 |
| L34 | `HEAD` on the project route | 200 with `Content-Type`, no `Content-Length` |

Covered by the server tests only, because they need a request held open or a failing mount:
a folder, a file or a symlink that takes the final name while an upload's body is still arriving
(409 `exists`, nothing replaced); a client that stops reading a project while a second project
is requested; `EREMOTEIO` mapping to 502 `store_error`.

Record the result here when the script is run:

| Date | Image tag / digest | Server tests | Smoke test | Notes |
|---|---|---|---|---|
| 2026-11-18 | local build, image id `sha256:5587dcb3046a` (not pushed) | pass as root and as uid 1000, `-race` clean, 91.1% of statements | 195 of 195 | The image built before `forbidden` was added fails L30 (2 of 195), as it should. |
| 2026-11-18 | local build, image id `sha256:482bb47e9bcd` (not pushed), after the review fixes (G0, no symlinks, commit re-check, `store_error`) | 60 passed and 1 skipped as root, 61 passed as uid 1000, `-race` clean, 91.4% of statements | 211 of 211 | The previous image (`5587dcb3046a`) fails 19 of the 211: L11, L31, L32 and L34. |

### 1.3 Browser pass (manual, or Playwright)

Not automated in this repository. Run it against the local container (`http://localhost:8080`)
in a current Chrome and Firefox. The fixture needs a tiled GeoTIFF (COG) and a FlatGeobuf with an
index: `ci/fixtures/make-range-fixtures.sh <dir>` adds both (it needs GDAL).

The Result column is the run of 2026-11-18: Playwright 1.49.1, headless Chromium with software
WebGL, the app running, service worker allowed except where noted. B1 to B22 ran on image
`5587dcb3046a`; B23 to B28 ran on `482bb47e9bcd`, together with a repeat of B1, B2, a GeoJSON
through "Open in new map", B13 by button and B15. Firefox and WebKit ran the overlay checks of
the first run only (the app does not start without WebGL there), so U9 stays open for them and
for a desktop browser with a GPU.

| # | Step | Expected | Result |
|---|---|---|---|
| B1 | Load `/` | App renders; "Data Store" tab visible on the right edge; no console errors from `/host/` | pass |
| B2 | Open the panel, browse Home | Roots listed in order; unavailable roots disabled; directories first | pass |
| B3 | Type in the filter box, including letters that are RustGIS shortcuts | Text appears in the box; the map does not react | pass: nothing reaches a listener registered after the overlay's; `x` and Ctrl+S on the canvas do reach RustGIS |
| B4 | In the filter box: Ctrl/⌘+A, Ctrl/⌘+C, Ctrl/⌘+V, Ctrl/⌘+Z | Normal text editing; RustGIS does not select all, paste or undo | pass |
| B5 | `Tab` / `Shift+Tab` round the panel; `Esc` | Focus stays inside the panel; `Esc` closes it and the canvas takes keys again | pass |
| B6 | Scroll the listing with the wheel; click on the backdrop | Map does not zoom or pan; backdrop click closes the panel | pass |
| B7 | Drag on the map, release over the closed panel's tab | The drag ends (no stuck pan) | not run |
| B8 | Tick the COG, **Open in new map** | Page reloads with `?data=…`; the layer draws; network panel shows 206 responses from `/fs/`, no full download | pass: 17 requests, all 206, 1.6 of 2.1 MB |
| B9 | Tick the FlatGeobuf, **Open in new map** | Layer draws; 206 responses | pass: 69 requests, all 206 |
| B10 | Tick 17 files | "Open in new map" disabled; status line says why | pass |
| B11 | A file whose name has a space, `+`, `&`, `#`, `%` and a non-ASCII letter: open it | Opens; the name is intact in the layer list | opens (200), but the layer is named `a%20b%2Bc%26d%23e%25f%20%C3%A9`: upstream names it from the undecoded address |
| B12 | **Copy URL**, then Add Data › From URL in RustGIS, paste | Layer is added to the current map | Copy URL: pass. Pasting it into Add Data › From URL: not run |
| B13 | Upload a file larger than two chunks by button, another by drag and drop | Progress advances; listing reloads; file is in the fixture directory with the same sha256 | pass by button (700,000 bytes in 3 chunks at `RGIS_MAX_CHUNK=262144`, sha256 equal on disk); drop tested with a synthetic event only |
| B14 | Start an upload, reload the page mid-way, choose the same file again | Resumes from the stored offset, not from 0 | pass (service worker blocked, to hold a request) |
| B15 | Upload a file whose name exists | Panel asks before replacing; "no" leaves the old file and no `.part` file | pass |
| B16 | Cancel an upload | Item is removed; no `.part` file remains | pass (service worker blocked): the item is marked Cancelled and its button becomes Remove; `DELETE` answered 204 and no `.part` file is left |
| B17 | New folder, with a name that contains `<b>` | Folder is created and shown as literal text | pass |
| B18 | In a read-only root | New folder and Upload are disabled | pass |
| B19 | Save a project in RustGIS that has a URL layer from `/fs`; upload the `.rgis`; restart the container on another port (`-p 127.0.0.1:8081:8080`, `REDIRECT_URL=http://localhost:8081`); **Open project** | Project opens and its layer loads from the new origin | pass with a COG and a FlatGeobuf layer, ports 18080 and 18081 (the second read-only): one `GET` of the project, then 206s from the new origin, nothing from the old. The project is named "World%20Atlas". A GeoJSON layer is stored inside the project, not by URL |
| B20 | Navigate the address bar to a `/fs/…` URL of an `.html` file placed in the fixture | Downloaded, not rendered | pass |
| B21 | Stop the container, then use the panel | Status line reports the failure; no uncaught errors | pass: "Network error" |
| B22 | Run with `RGIS_READONLY=1` | Browsing and opening work; no write control is enabled | pass |
| B23 | Tick one `.rgis`, then one `.rgispkg`: look at **Open in new map**; click it for the `.rgis`. Tick a project together with a data file | Same address as **Open project** (`?project=…`); the click fetches `/api/v1/project/…`. Mixed: disabled, and the status line says a project opens on its own | pass |
| B24 | Tick `roads.shp` with its `.dbf`, `.shx`, `.prj`, `.cpg`; the same plus another file; `roads.dbf` without `roads.shp` | One `data=` value, the `.shp`; parts left out beside other data; a part alone: disabled, with the hint to select the `.shp` | pass on the link (the fixture files were not a real Shapefile, so the layer itself was not loaded) |
| B25 | A file, and a folder, with `webgl` in the name: the launch link; open it. Then load `/?webgl` and tick any file | No `webgl` in the link's query string (`%77ebgl` instead); the file is fetched and drawn; the console shows the same WebGPU lines as a plain launch. With `/?webgl` the link still ends in `&webgl` | pass |
| B26 | Upload a file while the answer to its `commit=1` request is dropped after the server handled it (Playwright: `route.fetch()` then `route.abort()`) | The item ends as "Uploaded" with no Replace/Skip question: `HEAD`, `PUT` 409 `exists`, then `stat` 200 | pass (service worker blocked) |
| B27 | Upload with every `PUT` answered `502 {"error":{"code":"store_error"}}` (stubbed) | Two attempts, about 1 s apart, then the item fails with "The Data Store refused or failed the operation (permission, quota or connection)" | pass (service worker blocked) |
| B28 | A container with no `/data-store` | The panel's top level says the Data Store is not mounted and names the "Mount data store in analysis" option; only Session Scratch is enabled | pass |

## 2. In the DE (M2)

Register a **private** tool and app from `de/` and launch it. Fill in the table; do not share or
publish the app before the rows marked ◆ pass. "U" refers to `architecture.md` section 9. Where a
measurement contradicts the design, change `architecture.md` first, then the code.

Run details:

| | |
|---|---|
| Date | |
| Image tag and digest | |
| DE tool id / version | |
| DE app id | |
| Analysis host (`a….cyverse.run`) | |
| Browser(s) | |

### 2.1 Start-up and environment

| # | Measure | How | U | Result | Action needed |
|---|---|---|---|---|---|
| D1 | Image pull and time to "ready", first launch and a repeat launch | DE analysis timeline | | | |
| D2 | Environment the container actually gets | Not visible from the app; read `session`: `user`, `origin`. Cross-check `REDIRECT_URL` with DE support or a debug tool on the same DE | U1, U3 | | |
| D3 | `session.origin` equals the browser's `location.origin` (scheme included) | Browser console: no warning from the overlay; compare the two strings | U1 | | |
| D4 | `session.user` is the bare username and the Home root has `exists: true` | `session` | U3 | | |
| D5 | Which roots exist: home, shared, input, output, scratch | `session` | U4 | | |
| D6 | Contents and writability of the working directory; that `scratch` was created there | Scratch root `exists: true`; upload a small file to it | U4 | | |
| D7 | Readiness probe passes with the app's 200 on `/` | Analysis reaches "running" without a timeout | | | |

### 2.2 Proxy behaviour

| # | Measure | How | U | Result | Action needed |
|---|---|---|---|---|---|
| D8 ◆ | `Host`, `Origin` and `Sec-Fetch-Site` reach the app unchanged, and `Host` and the scheme are those of `REDIRECT_URL` | Same-origin use works with no 403, reads and writes (browse, New folder, upload); the analysis log has no `refused Host` line; then D9 and D10 | U1, U2 | | |
| D9 ◆ | A page on a **second analysis** cannot read `/fs` or `/api` | From another running analysis's page, in the same browser: `fetch("https://<this host>/api/v1/session", {credentials: "include"})` and the same for a `/fs/` URL. Expect 403 `forbidden_origin` (or a CORS failure), never the content | U2 | | |
| D10 ◆ | The same from an unrelated site, and by navigating to a `/fs/` URL from a link on another site | Expect 403 | U2 | | |
| D11 | `X-Forwarded-Proto` value seen by the app, if `REDIRECT_URL` is not usable | Only needed if D3 fails | U1, U2 | | |
| D12 | Largest request body the gateway passes | Upload a file over 8 MiB; note the chunk size the client settles at (network panel) | U6 | | |
| D13 | An early 409 or 413 reaches the browser as that status, not as a network error | Force an offset mismatch (start the same upload in two tabs) | U6 | | |
| D14 | Longest launch URL that works | Open 16 files with long paths; lengthen until it fails | U7 | | |
| D15 | Session expiry | Let the DE session lapse (or sign out in another tab), then use the panel and continue an upload: expect "Session expired; reload the page" and no upload data sent elsewhere | | | |
| D16 | No response of the app is turned into a redirect by the gateway | Network panel over a whole session: no 3xx on `/fs` or `/api` while signed in | | | |

### 2.3 Data Store mount

| # | Measure | How | U | Result | Action needed |
|---|---|---|---|---|---|
| D17 | Time to first render of a COG of 1 GB or more, against RustGIS's 30 s stall limit | Open in new map; time it; note the number and size of range requests | | | |
| D18 | Range-read latency on the mount: first byte and a mid-file range | Network panel timings for `/fs` 206 responses | | | |
| D19 | Listing time for a directory of about 1,000 and about 10,000 entries, first page and "more" | Network panel timing of `list` | U11 | | |
| D20 | Upload throughput with per-chunk reopen-and-append and `Sync` | Upload 100 MB and 1 GB to Home; MB/s; compare with the same to Scratch | U5 | | |
| D21 | Commit works for a new name (the hard link is expected to fail on irodsfs, the rename to follow), and over an existing file with replace | Upload new; upload again with replace; no `.rgis-upload-*.part` file left (DE Data window, hidden files shown) | U5 | | |
| D22 | Size on stat is current right after an append | A resumed upload reports the right `Upload-Offset` | U5 | | |
| D23 | Resume after the analysis's server restarted | Not normally possible to restart in the DE; resume after a page reload instead and note it | U5 | | |
| D24 | Ownership and visibility of files the app wrote | DE Data window: owner, permissions, and how soon they appear | U5 | | |
| D25 | Listing staleness: a file uploaded or deleted through the DE Data window while the analysis runs | Time until `list` reflects it | U11 | | |
| D26 | Writes to Community Data fail at the mount as well as at the service | Service answers 403 `read_only`; note separately whether the mount is read-only | U10 | | |
| D27 | Analysis Outputs: files written to `/output` are kept when the analysis ends | End the analysis with "save outputs"; look in the DE | U4 | | |
| D28 | How `.irodssymlink` or other special entries appear on the mount, if at all. Symlinks made on the mount by another tool must be absent from listings and answer 404 | Browse a directory known to hold iRODS links; make one with `ln -s` from another VICE app | | | |
| D36 | What a write the Data Store refuses looks like: upload into a folder the user may read but not write (and, if it can be arranged, over quota) | Expect `502 store_error` after one retry and the panel's "Data Store refused or failed" message; note the errno in the analysis log (`store error: …`). If it is not `EREMOTEIO`, extend the error mapping | U5 | | |
| D37 | The analysis launched with "Mount data store in analysis" **off** | The panel says the Data Store is not mounted; only Session Scratch is enabled. Note where staged inputs land and what is copied to the output folder at the end | U12 | | |
| D38 | Cost of the per-component `lstat` on `/fs` range reads | Compare D18 for a file at the top of Home and one six folders deep | U11 | | |

### 2.4 Sharing and the read-only variant

| # | Measure | How | U | Result | Action needed |
|---|---|---|---|---|---|
| D29 ◆ | What a shared-with user can do | Share the running analysis with a second account; as that user: open it, browse Home, upload | | | |
| D30 | How `RGIS_READONLY=1` can be set for the second app (app parameter, tool setting, or a separate image tag) | Try the definition in `de/`; confirm `session.readOnly` is `true` | | | |
| D31 ◆ | In the read-only app, a shared-with user cannot write | As D29, expect 403 `read_only` and disabled controls | | | |

### 2.5 App behaviour over the proxy

| # | Measure | How | U | Result | Action needed |
|---|---|---|---|---|---|
| D32 | Browser pass B1 to B18, B20 and B23 to B25, in the DE | Section 1.3 | U9 | | |
| D33 | Project portability across analyses: save a project with `/fs` layers in one analysis, open it in a new one | B19, with two analyses instead of two ports | U8 | | |
| D34 | The project is named from the file name | After D33, the name RustGIS shows | U8 | | |
| D35 | RustGIS's service worker does not serve stale `/host/` or `/api` content after an image update | Launch the new version in a browser that used the old one on the same host (only possible if the host name repeats; otherwise note "not applicable") | | | |

### 2.6 Outcome

| Question | Answer |
|---|---|
| Rows that failed | |
| Changes made to `architecture.md` as a result | |
| Environment variables that must be set on the tool | |
| Is the app safe to share and publish (D8, D9, D10, D29, D31)? | |
