# Architecture contract

This is the binding contract for `mesa-rustgis-app` milestones M1 and M3: the unpatched upstream
RustGIS v0.6.0 web bundle, the `rgis-host` file service, and the launch-mode overlay. Code and this
document must agree; change the document first.

**MUST**, **MUST NOT** and **MAY** are normative. **UNVERIFIED** marks an assumption about the
Discovery Environment (DE) that the M2 spike has to measure; section 9 lists them all.

## 1. Process model

One container, one process, one port.

- Entrypoint: `tini -g -- /opt/rustgis/bin/rgis-host`, as uid:gid `1000:1000`. No nginx, no iRODS
  credentials, no FUSE: the DE's CSI driver provides the mount.
- `rgis-host` with no arguments serves HTTP. `rgis-host healthcheck` requests
  `http://127.0.0.1:<port of RGIS_ADDR>/healthz` and exits 0 on a 200, 1 otherwise (the image
  `HEALTHCHECK`; the image has no curl).
- Go standard library only. `latest/server/go.mod` is `module rgis-host`, `go 1.27`, no
  dependencies; `package main` sits in `latest/server/` itself.
- **No redirects, ever.** `rgis-host` never answers 301, 302, 303, 307 or 308 on any route
  (upstream `main` refuses redirects on data requests). The only 3xx is `304`, in answer to
  a conditional request on a static file or `/fs`. So routing MUST NOT use `http.ServeMux`,
  `http.FileServer` or `http.ServeFile` (all three redirect unclean paths, directories or
  `index.html`); one top-level handler switches on the path prefix.
- Server timeouts: `ReadHeaderTimeout` 10 s, `IdleTimeout` 120 s, no read or write timeout (large
  transfers); the project route alone sets a write deadline (section 3). `SIGTERM`/`SIGINT` start
  a graceful shutdown with a 10 s limit.
- A missing Data Store, user or scratch directory is never fatal. `GET /` MUST answer 200 whenever
  the bundle is present.
- Logs go to stderr: one line per request with method, path, status, bytes and duration. No
  headers, no query strings. One exception: a request refused by guard rule G0 also logs the
  refused `Host` value, quoted, because nothing else shows what a gateway did to it.

### Environment

| Variable | Default | Meaning |
|---|---|---|
| `RGIS_ADDR` | `0.0.0.0:8080` | Listen address. |
| `RGIS_APP_DIR` | `/opt/rustgis/app` | The bundle: `index.html`, hashed `.js`/`.wasm`, `host/`. |
| `RGIS_DATA_STORE` | `/data-store` | Root of the CSI mount. |
| `RGIS_ZONE` | `iplant` | iRODS zone: first segment of the home and shared paths. |
| `RGIS_USER` | `$IPLANT_USER` | Owner's username. Valid when it matches `^[A-Za-z0-9._@-]{1,64}$`, does not start with `.` and is not `shared`. Otherwise the user is empty and the `home` root is left out. |
| `RGIS_SCRATCH` | `<working directory at start>/scratch` | Session scratch directory. Created at start (`0755`) unless read-only; failure is logged, not fatal. |
| `RGIS_READONLY` | unset | `1` or `true` (any case) makes every root read-only. Anything else is off. |
| `RGIS_PUBLIC_URL` | `$REDIRECT_URL` | The analysis's public URL. Usable when it parses as an absolute `http(s)` URL with a host; only its scheme and host are used. Set but empty means unusable (`REDIRECT_URL` is then ignored). Without a usable value the API answers only on loopback host names. |
| `RGIS_ALLOWED_HOSTS` | unset | Extra `host[:port]` values, comma-separated, accepted in `Host` and `Origin`. |
| `RGIS_MAX_CHUNK` | `8388608` | Largest upload chunk body, bytes (minimum 262144). |
| `RGIS_MAX_UPLOAD` | `0` | Largest uploaded file, bytes. `0` is unlimited. |
| `RGIS_MAX_PROJECT` | `268435456` | Largest file the project route rewrites, bytes. |
| `RGIS_VERSION` | `dev` | Image version (the `VERSION` file). Set by the Dockerfile. |
| `RGIS_UPSTREAM` | `unknown` | Upstream tag (`v0.6.0`). Set by the Dockerfile. |
| `RGIS_MODE` | `launch` | `launch` or `bridge`. The Dockerfile sets `bridge` only for a source build (M4). |

An invalid numeric value or `RGIS_MODE` is a start-up error (exit 2). The DE injects only
`REDIRECT_URL`, `IPLANT_USER` and `IPLANT_EXECUTION_ID` (app-exposer `vicebuild/deployments.go`).

### Derived values

- **Named hosts**: { host of `RGIS_PUBLIC_URL` when usable } ∪ `RGIS_ALLOWED_HOSTS`. Comparison
  is on the whole `host[:port]` string, ASCII case-insensitive, with no default-port
  normalisation.
- **Loopback hosts**: `localhost`, `127.0.0.1` and `[::1]`, alone or with any decimal port. No
  other spelling counts (`127.0.0.2`, `localhost.`, `x.localhost` do not).
- **Public scheme** `scheme(r)`: the first that applies: the scheme of `RGIS_PUBLIC_URL` when
  usable; the first `X-Forwarded-Proto` value when it is exactly `http` or `https`; `http`.
- **Public origin** `origin(r)` = `scheme(r)://<r.Host>`. The host is always the request's, because
  that is the one the browser used; guard rule G0 has checked it by the time it is used.

## 2. Roots and paths

### Root table

Roots are reported in this order. `<ds>` is `RGIS_DATA_STORE`, `<zone>` is `RGIS_ZONE`, `<user>` is
the valid user.

| id | label | Virtual path | URL under `/fs` | On-disk directory | Writable |
|---|---|---|---|---|---|
| `home` | `Home` | `/<zone>/home/<user>` | `/fs/<zone>/home/<user>/…` | `<ds>/<zone>/home/<user>` | yes |
| `shared` | `Community Data` | `/<zone>/home/shared` | `/fs/<zone>/home/shared/…` | `<ds>/<zone>/home/shared` | no |
| `input` | `Analysis Inputs` | `/input` | `/fs/input/…` | `<ds>/input` | no |
| `output` | `Analysis Outputs` | `/output` | `/fs/output/…` | `<ds>/output` | yes |
| `scratch` | `Session Scratch` | `/scratch` | `/fs/scratch/…` | `RGIS_SCRATCH` | yes |

- With the defaults, home and shared paths read as iRODS paths: `/iplant/home/alice/dem.tif`.
- `RGIS_READONLY` forces `writable: false` on every root.
- `exists` is evaluated per request, never captured at start: true when the directory exists and
  is a directory. A missing root is reported with `exists: false`; requests under it get
  `404 not_found`. It is never a start-up or session error, and a root that appears later works
  without a restart.
- The `home` root is omitted when the user is empty. Nothing else under `/<zone>/home` is reachable.

### Virtual path rules

A virtual path is the only way any route names a file. `/fs/<rest>` and `/api/v1/project/<rest>`
use `"/" + <rest>` taken from the percent-decoded URL path (`r.URL.Path`); other routes take a
`path` parameter.

A path is **valid** when all of these hold; otherwise the answer is `400 bad_path`:

1. Valid UTF-8, at most 4096 bytes, starts with `/`, and is not `/`.
2. No NUL, no backslash, no byte below `0x20`, no `0x7f`.
3. Split on `/`: no empty segment (so no `//` and no trailing slash), no `.` or `..` segment.
4. Each segment is at most 255 bytes, does not start with `.`, and does not end with
   `.irodssymlink` (ASCII case-insensitive).

Resolution of a valid path:

1. Pick the root whose virtual path equals the path or is a prefix of it on a segment boundary.
   None: `404 not_found`.
2. Resolve the remainder inside one `os.Root` opened on that root's directory. A missing entry
   and anything that is neither a regular file nor a directory answer `404 not_found`.
3. **No symlink is followed**, wherever it points. Each component below the root is checked with
   `lstat` before use; a path with a symlink in it answers `404 not_found`, and symlinks are not
   listed. The reason is rule 4 above: the rules see only the names in the request, so a link
   `key.txt -> .ssh/id_rsa` or `d -> .ssh` that was followed would reach a name they forbid.
   (The root directory itself may be a symlink; the `os.Root` still stops a lookup from leaving
   it.) The check and the open are separate system calls; the gap matters only to someone who
   can already create symlinks inside the root, and `os.Root` bounds what they can reach.
   Creating routes (`mkdir`, upload) treat a symlink at the final name as an existing entry:
   `409 exists`.

Names are byte-exact: no case folding, no Unicode normalisation.

## 3. HTTP API

### Conventions

- Every `/api` response body is JSON, `Content-Type: application/json`. Integers are JSON numbers.
  Times are RFC 3339 UTC with second precision (`2026-11-14T18:03:22Z`).
- No route uses a query parameter named `code` or the paths `/url-ready`, `/frontend-url`,
  `/active-sessions` or `/logout-user`: vice-proxy keeps them and they never reach the app.
- An **entry** is:

  ```json
  {"name": "dem.tif", "type": "file", "size": 734003200, "mtime": "2026-11-10T09:12:00Z"}
  ```

  `type` is `"file"` or `"dir"`. `size` is `0` for a directory.

- Every error on `/api` and `/fs` has this body (`HEAD` errors have none; a `416` from
  `http.ServeContent` keeps its plain-text body):

  ```json
  {"error": {"code": "not_found", "message": "no such file or directory"}}
  ```

  `message` is for people and is not part of the contract. Codes:

  | Status | `code` | When |
  |---|---|---|
  | 400 | `bad_request` | Missing or malformed parameter or body. |
  | 400 | `bad_path` | Path fails the validity rules. |
  | 400 | `not_a_directory` | `list` on a file. |
  | 403 | `forbidden_origin` | Guard rules G0, G1 or G2. |
  | 403 | `missing_header` | Guard rule G3. |
  | 403 | `read_only` | `RGIS_READONLY`, or the root is not writable. |
  | 403 | `forbidden` | The filesystem refused access (`EACCES`, `EPERM`, `EROFS`). |
  | 404 | `not_found` | No such root, entry or route; hidden or non-regular entry; directory on `/fs`. |
  | 405 | `method_not_allowed` | With an `Allow` header. Includes every `OPTIONS`. |
  | 409 | `exists` | Target exists (and `overwrite` is not set), or is not a regular file. |
  | 409 | `offset_mismatch` | Upload offset is not the temp file's size. Body also has `offset`. |
  | 409 | `size_mismatch` | Commit size differs from the bytes received. Body also has `offset`. |
  | 413 | `chunk_too_large` | Chunk body exceeds `RGIS_MAX_CHUNK`. Body also has `offset`. |
  | 413 | `file_too_large` | File exceeds `RGIS_MAX_UPLOAD` or `RGIS_MAX_PROJECT`. |
  | 415 | `bad_content_type` | Guard rule G4. |
  | 500 | `io_error` | Any other filesystem failure. |
  | 502 | `store_error` | `EREMOTEIO`: the Data Store mount refused or failed the operation. |
  | 507 | `no_space` | `ENOSPC` or `EDQUOT`. |

  irodsfs (the mount in the DE) reports every failure on the iRODS side as `EREMOTEIO`, a refused
  permission and an exceeded quota included (read from `irodsfs/fs_irods.go`). On the Data Store
  roots, `forbidden` is therefore reached only through `EROFS` (a read-only path mapping) and
  `EPERM`, and `no_space` is not reached at all; both remain what a local filesystem (scratch)
  answers. `store_error` does not say which of permission, quota or connection it was.

### Routes

| Method | Path | Class | Success |
|---|---|---|---|
| `GET`, `HEAD` | `/healthz` | static | 200, `text/plain; charset=utf-8`, body `ok\n` |
| `GET`, `HEAD` | `/`, `/index.html`, bundle files, `/host/*` | static | 200 (section 5) |
| `GET`, `HEAD` | `/fs/<path>` | read | 200, 206 |
| `GET` | `/api/v1/session` | read | 200 |
| `GET` | `/api/v1/list` | read | 200 |
| `GET` | `/api/v1/stat` | read | 200 |
| `GET`, `HEAD` | `/api/v1/project/<path>` | read | 200 |
| `HEAD` | `/api/v1/upload` | read | 200 |
| `POST` | `/api/v1/mkdir` | mutation | 201 |
| `PUT` | `/api/v1/upload` | mutation | 200, 201 |
| `DELETE` | `/api/v1/upload` | mutation | 204 |

Anything else under `/api/` or `/fs` is guarded as the read class and then answers `404 not_found`
(or `405` for a known path with another method). There is no delete, rename, move or copy route.
A query string that does not parse, or that gives a parameter more than once, is `400 bad_request`.

#### `GET /api/v1/session`

```json
{
  "user": "alice",
  "origin": "https://a1b2c3d4e.cyverse.run",
  "readOnly": false,
  "mode": "launch",
  "roots": [
    {"id": "home", "label": "Home", "path": "/iplant/home/alice", "writable": true, "exists": true},
    {"id": "shared", "label": "Community Data", "path": "/iplant/home/shared", "writable": false, "exists": true},
    {"id": "input", "label": "Analysis Inputs", "path": "/input", "writable": false, "exists": false},
    {"id": "output", "label": "Analysis Outputs", "path": "/output", "writable": true, "exists": true},
    {"id": "scratch", "label": "Session Scratch", "path": "/scratch", "writable": true, "exists": true}
  ],
  "limits": {"maxChunk": 8388608, "maxUpload": 0, "maxProject": 268435456, "maxOpen": 16},
  "versions": {"api": 1, "host": "0.6.0-1", "upstream": "v0.6.0"}
}
```

`user` is `""` when there is no valid user. `origin` is `origin(r)`. `mode` is `RGIS_MODE`.
`maxOpen` is the constant 16.

#### `GET /api/v1/list?path=<dir>[&cursor=<c>][&limit=<n>]`

```json
{
  "path": "/iplant/home/alice/project",
  "root": "home",
  "writable": true,
  "entries": [
    {"name": "rasters", "type": "dir", "size": 0, "mtime": "2026-11-14T18:03:22Z"},
    {"name": "dem.tif", "type": "file", "size": 734003200, "mtime": "2026-11-10T09:12:00Z"}
  ],
  "nextCursor": ""
}
```

- Hidden, never listed: names that start with `.` (upload temp files included), names that end
  with `.irodssymlink`, symlinks, entries that are neither regular files nor directories,
  entries whose `lstat` fails.
- Order: directories first, then files; within each by `strings.ToLower(name)` byte order, ties by
  `name`.
- `limit` defaults to 500; allowed 1–2000, anything else is `400 bad_request`.
- `cursor` is opaque to clients: pass `nextCursor` back unchanged. `""` means the first page on
  input and "no more" on output. Server form: the decimal count of entries to skip.
- Stateless: each call reads and sorts the whole directory.

#### `GET /api/v1/stat?path=<path>`

```json
{"path": "/iplant/home/alice/dem.tif", "root": "home", "writable": true,
 "entry": {"name": "dem.tif", "type": "file", "size": 734003200, "mtime": "2026-11-10T09:12:00Z"}}
```

For a root itself, `name` is the last segment of its virtual path.

#### `GET|HEAD /fs/<path>`

- Regular files only, through `http.ServeContent`: `Range` gives 206 or 416, `Accept-Ranges: bytes`,
  `Content-Length`, `Last-Modified`, conditional requests (304). Never a `Content-Encoding`.
- `ETag` is strong: `"<mtime in Unix nanoseconds, hex>-<size, hex>"`.
- A directory, `/fs` and `/fs/` answer `404 not_found`. No directory index.
- `Content-Type` comes only from this table, by lower-cased extension, and MUST be set before
  `ServeContent` so nothing is sniffed. No HTML, SVG or XML type is ever sent.

  | Extension | Type |
  |---|---|
  | `.tif`, `.tiff` | `image/tiff` |
  | `.json`, `.geojson`, `.rgis` | `application/json` |
  | `.csv` | `text/csv; charset=utf-8` |
  | `.txt` | `text/plain; charset=utf-8` |
  | `.png` | `image/png` |
  | `.jpg`, `.jpeg` | `image/jpeg` |
  | anything else | `application/octet-stream` |

#### `GET|HEAD /api/v1/project/<path>`

Serves a `.rgis` project with stale origins rewritten, so a project saved in one analysis opens in
the next (each analysis has a new origin, and projects keep URL layers by absolute address). The
path is in the URL, not the query, because upstream names the project from the last URL segment.

- The path MUST end in `.rgis` (ASCII case-insensitive), else `400 bad_request`. Larger than
  `RGIS_MAX_PROJECT`: `413 file_too_large`.
- `origin(r)` is written into the body unescaped, so this route answers `400 bad_request` unless
  the request `Host` matches `^[A-Za-z0-9.-]+(:[0-9]{1,5})?$` (an IPv6 literal is refused here).
- `GET` response: 200, the whole rewritten body, `Content-Type: application/json`,
  `Content-Length` set, no `Accept-Ranges`, no `ETag`, `Range` ignored.
- `HEAD` checks the path and the size limit and answers 200 with the `Content-Type` and no
  `Content-Length`: the length is known only after the rewrite, and the file is not read for it.
- The file is held in memory, so one read-and-rewrite runs at a time. That lock is released
  before the body is written to the client. The write has a deadline of 30 s plus one second per
  65536 bytes of body (a client slower than 64 KiB/s is cut off), so a client that stops reading
  cannot hold the buffer indefinitely; the deadline is cleared afterwards, because the connection
  may be kept alive for other routes.
- A body without the bytes `/fs/` is sent as read, with no second copy. Otherwise the rewrite
  makes one copy: a rewrite needs up to twice the file's size in memory for its duration.
- The rewrite is on bytes; the JSON is not parsed. Let `CUR` be `origin(r) + "/fs/"`. Every
  occurrence of each pattern below is replaced by `CUR`. Only the prefix through `/fs/` changes;
  the rest of the URL is untouched.
  1. The placeholder, the literal string `https://rgis-host.invalid/fs/`.
  2. If the host of `origin(r)`, port removed, matches `^a[0-9a-f]{8}\.(.+)$` with suffix `S`
     (an analysis host: `a` + 8 hex + base domain): the regular expression
     `https?://a[0-9a-f]{8}\.<S, quoted>(:[0-9]{1,5})?/fs/`.
  3. Else, if that host is `localhost` or `127.0.0.1`:
     `https?://(localhost|127\.0\.0\.1)(:[0-9]{1,5})?/fs/`.
- `.rgispkg` and other project formats are not rewritten; they are opened through `/fs`.
- Seen at v0.6.0: a saved `.rgis` keeps a URL only for layers read in place by range (COG,
  FlatGeobuf, ...). Layers that were downloaded whole (GeoJSON, CSV, ...) are stored inside the
  project, so they need no rewrite and no longer follow the file in the Data Store.

#### `POST /api/v1/mkdir`

Request body, at most 8192 bytes, `Content-Type: application/json`, an object with a string
`path` (other members are ignored):

```json
{"path": "/iplant/home/alice/new folder"}
```

Creates one directory (mode `0755`); the parent must exist. Response `201`:

```json
{"path": "/iplant/home/alice/new folder",
 "entry": {"name": "new folder", "type": "dir", "size": 0, "mtime": "2026-11-15T16:20:00Z"}}
```

Errors: root not writable `403 read_only`; parent missing `404 not_found`; target exists or is a
root `409 exists`.

#### Upload: `PUT|HEAD|DELETE /api/v1/upload`

Stateless and resumable. The server keeps no upload table: the temp file is the state, so an upload
resumes after a server restart.

| Parameter | On | Rule |
|---|---|---|
| `path` | all | Virtual path of the final file. Not a root (`400 bad_request`). |
| `id` | all | `^[0-9a-f]{32}$`, chosen by the client. |
| `offset` | `PUT` | Decimal, ≥ 0: where this chunk starts. |
| `commit` | `PUT` | Absent or `1`. `1` finishes the upload with this chunk. |
| `size` | `PUT` | Decimal total size. Required when `commit=1`, otherwise ignored. |
| `overwrite` | `PUT` | Absent or `1`. `1` allows replacing an existing regular file. |

Any other value for these parameters is `400 bad_request`.

- **Temp file**: `.rgis-upload-<id>.part`, in the destination directory (same filesystem as the
  final file, so commit is a rename). Mode `0644`. Listings hide it and no route can read it
  (path rule 4). It is append-only: the server never truncates or rewrites it.
- **`PUT`** body is the raw chunk, `Content-Type: application/octet-stream` (also when empty).
  Steps, in order; the first that fails answers:
  1. Guard (section 4), then parameters (`400`).
  2. Root lookup (`404 not_found`), root writable (`403 read_only`).
  3. The parent directory must exist: else `404 not_found`.
  4. The final path exists and `overwrite` is absent, or it is anything but a regular file (a
     directory, a symlink): `409 exists`.
  5. `cur` is the temp file's size, `0` when absent. `offset != cur`: `409 offset_mismatch` with
     `"offset": cur`.
  6. `Content-Length` over `RGIS_MAX_CHUNK`: `413 chunk_too_large` with `"offset": cur`. When
     `RGIS_MAX_UPLOAD` is set and `offset + Content-Length`, or `size`, exceeds it:
     `413 file_too_large`.
  7. Append the body at `offset`, then `Sync`, then `Close`; all three are checked. A body that
     runs past `RGIS_MAX_CHUNK` stops there with `413 chunk_too_large`; a body that ends before
     its declared length answers `400 bad_request`. Whatever was written stays (it is valid file
     content), and every `offset` in a response is the temp file's size at that moment.
  8. Without `commit`: `200`.
  9. With `commit=1`: if the temp size differs from `size`, `409 size_mismatch` and the temp file
     stays. Otherwise, under a lock on the final name (one commit per name at a time in this
     process):
     1. Repeat the check of step 4, which is by now as old as the body took to arrive:
        `409 exists`, and the complete temp file stays, so the client can send an empty
        `offset=<size>&commit=1&overwrite=1` once the user agrees.
     2. Without `overwrite`: hard-link the temp file to the final name and remove the temp name.
        A link never replaces, whoever created the name in between (`409 exists`). If the
        filesystem has no hard links (irodsfs), fall through to the rename; a name created
        *outside this process* between the check and the rename is then replaced.
     3. Otherwise rename the temp file to the final name. `rename(2)` replaces a regular file
        and refuses a directory. A failed rename is reported and nothing is removed: the server
        never deletes the existing file itself.

     Then answer `201`.

  ```json
  {"offset": 8388608, "committed": false}
  ```

  ```json
  {"offset": 20971520, "committed": true, "path": "/iplant/home/alice/dem.tif",
   "entry": {"name": "dem.tif", "type": "file", "size": 20971520, "mtime": "2026-11-15T16:21:07Z"}}
  ```

  ```json
  {"error": {"code": "offset_mismatch", "message": "expected offset 4194304"}, "offset": 4194304}
  ```

  - A `PUT` with `offset` equal to the temp size, `commit=1` and an empty body commits what is
    there. An empty file is `offset=0&commit=1&size=0`.
  - `PUT`s for one temp file are serialised in the process; a concurrent duplicate therefore gets
    `409 offset_mismatch`. Commits to one final name are serialised too, across upload ids.
  - The server keeps nothing about an upload once it is committed. A client whose commit got no
    answer finds `Upload-Offset: 0` and then `409 exists`; it settles that with `stat` (section 6,
    upload step 7).
  - Before answering a `PUT` that fails in steps 1–6, the server reads and discards up to
    16777216 bytes of the body, so the browser receives the status instead of a connection reset.
- **`HEAD`** answers `200` with the header `Upload-Offset: <temp file size, 0 when absent>`. It
  runs steps 1–3 without the writable check.
- **`DELETE`** (abort) removes the temp file and answers `204`, also when there was none. It runs
  steps 1–3 and can remove nothing but `.rgis-upload-<id>.part`.
- Abandoned temp files are not collected in v1.

## 4. Guard and response headers

The guard exists because vice-proxy answers CORS with credentials for every sibling analysis
(`*.cyverse.run`) and forwards no user identity. It runs before routing, on every `/fs` and `/api`
request, `GET` and `HEAD` included. The static class is not guarded.

| Rule | Applies to | Reject when | Answer |
|---|---|---|---|
| G0 | read, mutation | The request `Host` is neither a named host nor a loopback host (section 1) | `403 forbidden_origin` |
| G1 | read, mutation | `Sec-Fetch-Site` is present and is neither `same-origin` nor `none` | `403 forbidden_origin` |
| G2 | read, mutation | `Origin` is present and is `null`, unparsable, its scheme is not `scheme(r)`, or its host is neither the request `Host` nor a named host | `403 forbidden_origin` |
| G3 | mutation | `X-RGIS-Host` is not exactly `1` | `403 missing_header` |
| G4 | `POST`, `PUT` | Media type (parameters ignored) is not `application/json` for `POST`, `application/octet-stream` for `PUT`, or the header does not parse | `415 bad_content_type` |
| G5 | mutation | `RGIS_READONLY` is on | `403 read_only` |

- Rules run in that order; the per-root writable check comes later, after the path resolves.
- A request with neither `Sec-Fetch-Site` nor `Origin` passes G1 and G2 (non-browser clients, the
  kubelet, older browsers on a same-origin `GET`).
- G3 and G4 make every mutation a non-simple request: a form cannot send it, and a cross-origin
  script needs a preflight that neither vice-proxy nor `rgis-host` approves.
- `rgis-host` never sends an `Access-Control-*` header and never approves a preflight: an
  `OPTIONS` that passes G1 and G2 answers `405`, any other `403 forbidden_origin`.
- G0 is the defence against DNS rebinding. A page on a name the attacker controls, re-pointed at
  this server's address, is same-origin as far as the browser can tell: it sends
  `Sec-Fetch-Site: same-origin` and an `Origin` equal to its `Host`, so G1 and G2 pass it. Its
  `Host` is the attacker's name, which G0 refuses. In the DE vice-proxy's host-bound cookie
  already stops this; a local run has only G0.
- Consequences of G0: with no usable `RGIS_PUBLIC_URL` the API answers only on loopback names. To
  reach a local run by another name or address, set `RGIS_PUBLIC_URL` (or `RGIS_ALLOWED_HOSTS`)
  to it. A gateway that rewrites `Host` needs `RGIS_ALLOWED_HOSTS` (U2).

Response headers:

| Class | Headers |
|---|---|
| every response | `X-Content-Type-Options: nosniff` |
| static | `Cache-Control` per section 5; `Content-Security-Policy: frame-ancestors 'self'` |
| `/fs` | `Content-Disposition: attachment`; `Content-Security-Policy: sandbox; default-src 'none'; frame-ancestors 'none'`; `Cross-Origin-Resource-Policy: same-origin`; `Cache-Control: private, no-cache` |
| `/api` | `Content-Security-Policy: sandbox; default-src 'none'; frame-ancestors 'none'`; `Cross-Origin-Resource-Policy: same-origin`; `Cache-Control: no-store` |
| `/api/v1/project` | the `/api` set, plus `Content-Disposition: attachment` |

Error responses carry their class's headers.

## 5. Static files

Mirrors upstream `packaging/docker/nginx.conf` at v0.6.0.

- Served from `RGIS_APP_DIR`. `/` and `/index.html` both serve `index.html`; any query string is
  ignored. Any other path that is a directory, is missing, has a `.`-prefixed segment, or ends in
  `.gz` answers 404 (plain text). No fallback to `index.html`, no directory index.
- `GET` and `HEAD` only; anything else is 405.
- `Cache-Control`: `public, max-age=31536000, immutable` when the URL path matches
  `-[0-9a-f]{8,}(_bg)?\.(js|wasm)$`; `no-cache` for everything else, `index.html`, `sw.js` and
  `/host/*` included.
- Precompressed siblings: when `<file>.gz` exists and `Accept-Encoding` lists `gzip` without
  `q=0`, serve the `.gz` bytes with `Content-Encoding: gzip` and the original file's
  `Content-Type`. `Vary: Accept-Encoding` is sent whenever a `.gz` sibling exists. The bundle's
  subresource-integrity hashes still pass, because the browser checks the decoded bytes.
  A compressed response carries the `.gz` file's `Content-Length` (so the 17 MB wasm download
  shows progress) and ignores `Range`: it is always the whole file, `200`.
- `ETag` as for `/fs`, computed from the file actually sent.
- `Content-Type` by extension, from this table only:

  | Extension | Type |
  |---|---|
  | `.html` | `text/html; charset=utf-8` |
  | `.js` | `text/javascript; charset=utf-8` |
  | `.css` | `text/css; charset=utf-8` |
  | `.wasm` | `application/wasm` |
  | `.webmanifest` | `application/manifest+json` |
  | `.json` | `application/json` |
  | `.txt`, no extension | `text/plain; charset=utf-8` |
  | `.png` | `image/png` |
  | `.svg` | `image/svg+xml` |
  | anything else | `application/octet-stream` |

- Upstream's service worker (`sw.js`) is served unchanged. It caches only the app shell and leaves
  `/fs`, `/api` and `/host` requests to the browser.

## 6. Overlay

Two files, `latest/host/rustgis-host.js` and `latest/host/rustgis-host.css`, served at
`/host/rustgis-host.js` and `/host/rustgis-host.css`.

### Injection (`latest/inject.sh`)

`inject.sh <path to index.html>`, POSIX `sh`, using only `grep`, `sed` or `awk`.

- Inserts exactly this text immediately before `</head>`:

  ```html
  <link rel="stylesheet" href="/host/rustgis-host.css" data-rgis-host><script src="/host/rustgis-host.js" data-rgis-host></script>
  ```

- The script is a classic, parser-blocking script: no `defer`, `async` or `type="module"`. It
  therefore runs before upstream's module script, which the key rule below depends on.
- Idempotent: if the file already contains `data-rgis-host`, it exits 0 and changes nothing.
- Exits 1, changing nothing, unless `</head>` occurs exactly once. (v0.6.0: once.)
- Nothing else in the bundle is modified. The image gzips `index.html` after injection.

### Script rules

- One classic script: no modules, no build step, no dependencies, no network access other than
  same-origin `/api` and `/fs`. It runs as an IIFE in strict mode and adds exactly one global,
  `window.rustgisHost`. It runs in `<head>`, so it builds its DOM on `DOMContentLoaded`.
- File and folder names are untrusted. DOM is built with `createElement` and `textContent`; never
  `innerHTML` with data.
- Every `/api` request sends `X-RGIS-Host: 1`, with `credentials: "same-origin"`,
  `cache: "no-store"` and `redirect: "manual"`. `PUT` always sets
  `Content-Type: application/octet-stream`; `POST` sets `application/json`.
- A response of type `opaqueredirect` means the DE session expired (vice-proxy redirects to the
  login). The overlay stops what it was doing and says "Session expired; reload the page".
- An error body that is not this contract's JSON (a gateway's or vice-proxy's) is reported by its
  status alone.
- Its only `localStorage` key is `rgis-host.dock` (where the user dragged the toggle). It does
  not read or write keys starting with `rustgis.`, and does not touch
  upstream's elements (`#rustgis_canvas`, `#rustgis_loading`, `#rustgis_update`, `#rfd-overlay`)
  other than focusing the canvas.
- The overlay always builds URLs from `location.origin`. If `session.origin` differs, it logs one
  `console.warn` (the project rewrite would then target the wrong origin).
- CSS: every selector starts with `#rgis-host-root`. No rules for `html`, `body` or `canvas`. No
  external fonts or images. `position: fixed`, `z-index: 1000`.

### DOM

Every id and class starts with `rgis-host-`. One root is appended to `document.body`. These ids are
fixed (tests use them):

| Id | Element |
|---|---|
| `rgis-host-root` | Root `div`. Carries class `rgis-host-launch` or `rgis-host-bridge`. |
| `rgis-host-toggle` | `button` "Data Store": a small tab on the right edge, always visible. It can be dragged up and down, clear of upstream's controls. |
| `rgis-host-backdrop` | Full-viewport layer under the panel while it is open; a click closes the panel. |
| `rgis-host-panel` | Drawer from the right, `role="dialog"`, `aria-modal="true"`, `hidden` when closed. Width `min(480px, 100vw)`. |
| `rgis-host-close` | `button` that closes the panel. |
| `rgis-host-refresh` | `button` that reloads the session (roots can appear after start) and the current listing. |
| `rgis-host-roots` | Root list. A root with `exists: false` is shown disabled. |
| `rgis-host-crumbs` | Breadcrumb of the current directory. |
| `rgis-host-filter` | `input type="text"`: filters the loaded listing by name, case-insensitive. |
| `rgis-host-list` | Listing. Each row has class `rgis-host-row`, `data-path` (virtual path) and `data-type` (`file` or `dir`). File rows have a checkbox. |
| `rgis-host-more` | `button` that loads the next page; present only when `nextCursor` is not empty. |
| `rgis-host-mkdir-name`, `rgis-host-mkdir` | `input type="text"` and `button` for New folder. |
| `rgis-host-upload`, `rgis-host-upload-input` | `button`, and a hidden `input type="file" multiple`. |
| `rgis-host-uploads` | Upload list. Each item has class `rgis-host-upload-item`, a `progress` element and a cancel `button`. |
| `rgis-host-open` | `a` "Open in new map". |
| `rgis-host-open-project` | `a` "Open project". |
| `rgis-host-copy` | `button` "Copy URL". |
| `rgis-host-status` | `role="status"` message line. |

### `window.rustgisHost`

```js
window.rustgisHost = {
  version: 1,                  // this contract
  mode: "launch",              // or "bridge"
  placeholder: "https://rgis-host.invalid/fs/",
  session: null,               // GET /api/v1/session body once loaded
  ready,                       // Promise<void>, resolves when session is loaded
  open(), close(), toggle(),   // panel visibility
  navigate(path),              // Promise<void>: show a directory
  fileUrl(path),               // string
  projectUrl(path),            // string
  launchUrl(paths),            // string; throws RangeError outside 1..16 paths or over 6000 chars
  projectLaunchUrl(path),      // string
  upload(files, dirPath),      // Promise<entry[]>; files is a FileList or File[]
  mkdir(path),                 // Promise<entry>
};
```

`pick` and `save` are deliberately absent in launch mode (section 7). `upload` settles when every
file has: it resolves with the entries of the committed files (cancelled and skipped files are
left out) and rejects with the first error if any file failed.

URL building is normative:

```js
const encPath = (p) => p.split("/").map(encodeURIComponent).join("/");
const queryValue = (u) => encodeURIComponent(u).replace(/webgl/g, "%77ebgl");
fileUrl(p)          = location.origin + "/fs" + encPath(p)
projectUrl(p)       = location.origin + "/api/v1/project" + encPath(p)
launchUrl(ps)       = location.origin + "/?" + ps.map((p) => "data=" + queryValue(fileUrl(p))).join("&") + keep
projectLaunchUrl(p) = location.origin + "/?project=" + queryValue(projectUrl(p)) + keep
```

- The address is encoded twice on purpose: once per path segment to form the URL, once as a whole
  to form the query value. Upstream splits the query on `&` and applies `decodeURIComponent` once,
  leaving `+` literal (v0.6.0 `web.rs:278`); `encodeURIComponent` never emits a bare `+`, `&` or
  `#`. Example: `/iplant/home/alice/a b+c.tif` gives
  `data=https%3A%2F%2Fhost%2Ffs%2Fiplant%2Fhome%2Falice%2Fa%2520b%252Bc.tif`.
- `queryValue` hides the word `webgl`: upstream switches to its WebGL backend when the raw query
  string *contains* `webgl` anywhere (v0.6.0 `web.rs:114`, a substring test, not a parameter
  lookup), so a file or folder named `webgl-demos` would silently turn WebGPU off. `%77` is `w`;
  upstream's `decodeURIComponent` restores the name.
- `keep` carries over `webgl`, `gpu` and `lang` from the current `location.search`: each matching
  raw segment (`webgl`, `gpu=0`, `lang=de`) is appended as `"&" + segment`, unchanged.
- Upstream reads at most 16 `data=` values, requires absolute `http(s)` URLs, and ignores `data=`
  when `project=` is present.
- A launch URL longer than 6000 characters is refused (request-line limits at the gateway).

### Launch-mode behaviour

- **Start**: load the session, render the toggle, stay closed. A failed session load shows the
  toggle and an error in the status line when opened.
- **Browse**: the top level is the root list. Opening a root or directory calls `list`; directory
  rows navigate, file rows toggle selection. Selection is cleared on navigation.
- **No Data Store**: when no root other than `scratch` has `exists: true`, the top level says
  that the Data Store is not mounted and that the app must be launched with the DE's "Mount data
  store in analysis" option on (U12).
- **Open in new map**: its `href` is kept current as the selection changes; a plain click
  navigates this tab, and the browser's own modifiers open a new tab. When it cannot act it has
  no `href`, `aria-disabled="true"`, and the status line says why. The panel states that the
  current map is replaced; upstream itself asks before leaving a map with unsaved changes. The
  selection, in listing order, decides:
  1. Nothing selected: disabled.
  2. A project file (`.rgis` or `.rgispkg`, case-insensitive) selected alone: the same `href` as
     **Open project**. Upstream opens a project only through `?project=` (given as `?data=` it is
     parsed as a data file and fails) and ignores `?data=` beside it, so a project selected
     together with anything else: disabled.
  3. Shapefile parts: a selected `.dbf`, `.shx`, `.prj` or `.cpg` (case-insensitive) whose `.shp`
     of the same name is also selected is left out, and does not count towards the 16. Upstream
     fetches the parts itself from the `.shp` address (`crates/data/src/io.rs`) and refuses a
     part on its own, so a part selected without its `.shp`: disabled.
  4. More than 16 files left, or an over-long URL: disabled. Otherwise `launchUrl(files)`.
- **Open project**: enabled with exactly one file selected. For `.rgis` (case-insensitive) the
  `href` is `projectLaunchUrl(path)`; for `.rgispkg` it is
  `location.origin + "/?project=" + queryValue(fileUrl(path)) + keep`. Otherwise disabled.
- **Names** (seen at v0.6.0): upstream names a layer or project from the last URL segment
  without decoding it, and refuses an address with a raw space. `my data.geojson` therefore
  becomes the layer "my%20data", and `World Atlas.rgis` the project "World%20Atlas". Launch
  mode cannot change that; names made of `A-Z a-z 0-9 - _ .` read as written.
- **Copy URL**: enabled with exactly one file selected. Writes `fileUrl(path)` with
  `navigator.clipboard.writeText`; if that fails, the status line shows the URL in a read-only
  input, focused and selected. (The URL works in this analysis only: in the app's Add Data › From
  URL, not from other machines.)
- **New folder**: enabled when the current directory is writable. Sends
  `mkdir(current + "/" + name)`, then reloads the listing. Server errors go to the status line.
- **Upload**: enabled when the current directory is writable. Files come from the file input or
  from a drop on the panel. Files upload one at a time, chunks in sequence:
  1. `path = dir + "/" + file.name`. `id` is the first 32 hex characters of the SHA-256 of
     `path + "\n" + file.size + "\n" + file.lastModified` (UTF-8), so choosing the same file again
     resumes it. Without `crypto.subtle`, `id` is 32 random hex characters.
  2. A file over `limits.maxUpload` (when not 0) fails before any request.
  3. `HEAD` gives the starting offset. If it exceeds `file.size`: `DELETE`, start at 0.
  4. Chunk size starts at `min(limits.maxChunk, 8388608)`. The chunk that reaches `file.size`
     carries `commit=1&size=<file.size>`.
  5. `200`/`201`: take `offset` from the body. `409 offset_mismatch`, `409 size_mismatch`,
     `413 chunk_too_large`: take `offset` from the body and continue from there.
  6. Any `413` other than `file_too_large` (a gateway's included): halve the chunk size; at
     262144 bytes a further 413 fails the file.
  7. `409 exists`: ask in the panel whether to replace; yes resends with `overwrite=1`, no sends
     `DELETE` and skips the file. One case is settled without asking. If a `commit=1` request of
     this transfer ended in a network error or a 5xx, its answer may have been lost after the
     server committed, and the retry then meets the client's own upload under the final name. The
     client calls `stat`: a regular file of exactly `file.size` whose `mtime` is not older than
     the start of this transfer is taken as the committed upload. The start is the `Date` header
     of the transfer's first response (the server's clock, not the browser's), less 2 s for
     rounding. If the mount's clock disagrees by more, the question is asked as before.
  8. Network error or 5xx other than `507 no_space`: retry up to 5 times, waiting 1, 2, 4, 8,
     16 s, each retry starting again from `HEAD`. `502 store_error` is retried once only: on the
     Data Store it is what a refused permission or an exceeded quota looks like, and the message
     says so. A 401, 403 or `opaqueredirect` stops all uploads.
  9. Cancel aborts the request and sends `DELETE`.
  10. `progress` shows bytes confirmed or sent over `file.size`, updated at least once per chunk.
      After a commit the listing reloads.
  11. While an upload is queued or running the overlay holds a `beforeunload` warning, because
      "Open in new map" navigates the tab.

### Keyboard and focus

Typing, clicking and scrolling in the overlay must not reach the egui canvas, and upstream must
not swallow editing keys meant for overlay inputs. Read from eframe 0.37.0
(`crates/eframe/src/web/events.rs`) and v0.6.0 `web.rs`: eframe takes `keydown`, `keyup`,
`pointerdown`, `wheel`, touch-start and drops on the **canvas** only; clipboard events and
pointer/touch move and release on `document`, acting only while the canvas has focus or the
pointer is over it; and upstream adds a **`window` capture-phase `keydown`** listener that calls
`preventDefault()` on its Ctrl/⌘ shortcuts (`web.rs:1277`).

- At script evaluation (before upstream starts) the overlay adds one `window` capture-phase
  `keydown` listener. For an event whose target is inside `#rgis-host-root` it calls
  `stopImmediatePropagation()` and does all of the overlay's own key handling there: `Escape`
  closes the panel, `Tab`/`Shift+Tab` cycle inside it, `Enter` in `#rgis-host-mkdir-name` creates
  the folder. No other `keydown` listener is used. `preventDefault()` only for those handled keys.
  One exception: while the panel is open, `Escape` and `Tab` whose target is `<body>` or `<html>`
  are handled the same way, because re-rendering can drop focus there and the panel is modal.
- `#rgis-host-root` has bubble-phase listeners that call `stopPropagation()` for `copy`, `cut`,
  `paste`, `dragover` and `drop`, and `preventDefault()` for `dragover`/`drop`.
- `pointermove`, `pointerup`, `mousemove`, `mouseup`, `touchmove`, `touchend` and `touchcancel`
  are never stopped: eframe ends canvas drags from `document`.
- The panel is modal. While it is open the backdrop covers the whole viewport, so no pointer event
  lands on the canvas. Opening moves focus to `#rgis-host-filter`; a `focusin` on `document` whose
  target is outside `#rgis-host-root` moves focus back to the element last focused inside.
- Closing the panel calls `document.getElementById("rustgis_canvas")?.focus({preventScroll: true})`.
- The overlay registers no global keyboard shortcut.

## 7. Bridge-mode seam (M4, not built now)

- **Detection**: on start and again on the window event `TrunkApplicationStarted`, the overlay sets
  `mode = "bridge"` and swaps the root class when `window.rustgis_host_run` or
  `window.wasmBindings.rustgis_host_run` is a function. Until M4 that is the only effect; every
  launch-mode behaviour stays.
- **Reserved for M4**: `rustgisHost.pick(purpose, exts)` and `rustgisHost.save(name, blob)`. The
  patched app calls them only when they are functions and otherwise uses upstream's own picker and
  download, so they MUST stay undefined until implemented.
  `rustgis_host_run(command, paramsJson)` is the reserved export name.
- **Placeholder**: `save` will replace `location.origin + "/fs/"` with `rustgisHost.placeholder`
  in `.rgis` text before upload. The project route already rewrites it.
- **Dockerfile**: `ARG RUSTGIS_SOURCE=zip`. Any other value fails the build with
  `RUSTGIS_SOURCE=<value> is not implemented until M4`.
- **`scripts/upstream.sh`**: subcommands `checkout`, `apply`, `refresh`, `bump`, `rebuild`.
  `bump [TAG]` and `rebuild` are implemented (`bump` rewrites `UPSTREAM_REF`; both rewrite
  `VERSION` and the version fields of `de/*.json`); the others print `not implemented until M4`
  to stderr and exit 2.
- `patches/` and `.github/workflows/upstream-bump.yml` are not created in this run.

## 8. Build interfaces and file ownership

Shared facts the builders rely on:

- Names: image `harbor.cyverse.org/vice/mesa-rustgis:<VERSION>` (and `:latest`), DE tool
  `mesa-rustgis`, DE app `MESA RustGIS`, image user `rustgis` (1000:1000), port 8080.
- Build context is `latest/` (as in `../rstudio`). The Dockerfile copies `server/`, `host/` and
  `inject.sh` from it. `VERSION` and `UPSTREAM_REF` are outside the context and reach the build
  as build args.
- `VERSION`: one line, `<upstream version>-<n>`, now `0.6.0-1`. `de/tool.json` (`version`,
  `container.image.tag`) and `de/app*.json` (`version`, `tools[0].version`) carry the same
  value; `pr.yml` fails when they differ.
- `UPSTREAM_REF`: shell-sourceable `KEY=value` lines; the build args have the same names:

  ```
  RUSTGIS_TAG=v0.6.0
  RUSTGIS_COMMIT=6db08fe7203a54d568deeefcc21f6a7dcc38f26a
  RUSTGIS_WEB_ZIP_SHA256=302bbf07f3fd4025fcb1c37e37df819f318a2ec8668b1cbabeb4037cdc07d152
  ```

  The zip is
  `https://github.com/opengeos/RustGIS/releases/download/v0.6.0/RustGIS-0.6.0-web.zip` (flat
  archive, 17,171,884 bytes).
- Image layout: `/opt/rustgis/bin/rgis-host`; `/opt/rustgis/app/` holds the unzipped bundle, the
  injected `index.html`, and `host/rustgis-host.{js,css}`; `WORKDIR /home/rustgis/data-store`
  (empty: the DE mounts a per-analysis volume there); `ENV HOME=/home/rustgis`.
- After injection and after `host/` is copied, the image runs `gzip -9 -k` on every `.wasm`,
  `.js`, `.css`, `.html`, `.txt` and `.webmanifest` file under `/opt/rustgis/app`.
- The image sets `RGIS_VERSION` (from `VERSION`), `RGIS_UPSTREAM` (from `RUSTGIS_TAG`) and
  `RGIS_MODE=launch`.
- The server is built with `golang:1.27.3` (the fleet's `GO_VERSION`) in `server/`:
  `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o <out>/rgis-host .`, after
  `go vet ./... && go test ./...`. Tests need no network, use only `t.TempDir()`, and pass both as
  root and as uid 1000.
- Go is compiled and tested only in a container:
  `docker run --rm -v "$PWD/latest/server:/src" -w /src golang:1.27.3 sh -c 'go vet ./... && go test ./...'`.
  `make test-server` does this with the exact image (digest included) named in the Dockerfile's
  `host` stage, as root and as uid 1000.
- Local run like the DE:
  `docker run --rm --user 1000 -p 127.0.0.1:8080:8080 -e IPLANT_USER=<user> -e REDIRECT_URL=http://localhost:8080 -v <fixture>:/data-store <image>`.
  Publish the port on `127.0.0.1` only: the server has no login. With `--read-only`, mount a
  writable volume or tmpfs at the working directory (scratch lives there).
- Publishing: `make push` (`scripts/push.sh`) pushes `:latest`, and `:<VERSION>` only when the
  registry answers "not found" for it; any other answer fails. A published version tag is never
  moved.
- Fixture Data Store (`ci/fixtures/make-data-store.sh <dir>`): user `rgistest`, with
  `<dir>/iplant/home/rgistest`, `<dir>/iplant/home/shared`, `<dir>/input` and `<dir>/output`.
- Copyright holder text: `The MESA RustGIS App authors`. No personal names or email addresses in
  any file.

Each builder creates only its own paths:

| Builder | Paths |
|---|---|
| SERVER | `latest/server/**` |
| OVERLAY | `latest/host/**` |
| IMAGE | `latest/Dockerfile`, `latest/.dockerignore`, `latest/inject.sh`, `VERSION`, `UPSTREAM_REF`, `Makefile`, `scripts/**` (`upstream.sh`, `push.sh`), `ci/**`, `de/**`, `.github/**` (workflows, `dependabot.yml`), `.gitignore`, `.hadolint.yaml`, `.trivyignore` |
| DOCS | `README.md`, `LICENSE`, `NOTICE`, `docs/security.md`, `docs/verification.md`, `docs/upstream-updates.md` |

`docs/architecture.md` (this file) is changed only by agreement of all four.

## 9. Unverified assumptions

Each has a safe fallback in the design; M2 measures them in the DE.

| # | Assumption | Fallback if wrong |
|---|---|---|
| U1 | `REDIRECT_URL` is the analysis's public URL in the deployed DE (true at app-exposer HEAD), so its scheme and host are the browser's. If the host is wrong, G0 refuses every `/fs` and `/api` request; if the scheme is wrong, G2 refuses every request that carries `Origin` (all writes). | Set `RGIS_PUBLIC_URL` on the tool; `RGIS_ALLOWED_HOSTS` for a host alone. (An empty `RGIS_PUBLIC_URL` leaves only loopback names.) The log names each refused `Host`. |
| U2 | The gateway passes `Host`, `Origin`, `Sec-Fetch-Site` and `X-Forwarded-Proto` through unchanged (vice-proxy does: read from its source). | A rewritten `Host` is refused by G0 until it is listed in `RGIS_ALLOWED_HOSTS`. If `Sec-Fetch-Site` is stripped, G2 still refuses cross-origin CORS requests. |
| U3 | `IPLANT_USER` is the bare username that names `/data-store/iplant/home/<user>`. | `RGIS_USER`; an unusable value only drops the `home` root. |
| U4 | Mount layout: `/data-store/{input,output,iplant/home/<user>,iplant/home/shared}`; the working directory is a writable per-analysis volume. | `RGIS_DATA_STORE`, `RGIS_ZONE`, `RGIS_SCRATCH`; missing roots report `exists: false`. |
| U5 | irodsfs handles reopen-and-append, `Sync`, rename (including over an existing file: read from `IRODSRename`, which removes the destination itself) and a fresh size on stat at usable cost. It has no hard links, so the no-overwrite commit falls back to check-then-rename. Its failures arrive as `EREMOTEIO` (`store_error`), with no errno for permission or quota. | Stage uploads in scratch and copy once: a server-only change, same protocol. |
| U6 | An 8 MiB request body passes the gateway, and an early `409`/`413` reaches the browser. | The client halves on 413 down to 256 KiB and re-reads the offset after a network error; `RGIS_MAX_CHUNK` lowers the start. |
| U7 | A launch URL of up to 6000 characters passes the gateway and vice-proxy. | Lower the constant in the overlay. |
| U8 | Upstream opens `?project=` with one plain `GET` and names the project from the last URL segment, percent-encoding kept (read from v0.6.0 `lib.rs:3133`, `5377`; confirmed in the local browser pass, not yet through vice-proxy). | Serve the rewritten body through `http.ServeContent`. |
| U9 | The overlay behaves over the running app as the eframe 0.37.0 and v0.6.0 sources say (confirmed in headless Chromium in the local browser pass; Firefox and Safari with the app running are still open). | The browser pass in `docs/verification.md` checks typing, Ctrl+A and paste in overlay inputs. |
| U10 | `/data-store/iplant/home/shared` is read-only in effect. | The service treats it as read-only regardless. |
| U11 | Listing a large directory on irodsfs (an `lstat` per entry, repeated per page) is fast enough, and so is the `lstat` per path component that every request makes (section 2). | Cache the sorted listing per directory for a few seconds: a server-only change. |
| U12 | The analysis is launched with the DE's "Mount data store in analysis" option on (the default: sonora sends `mount_data_store ?? true`). With it off, app-exposer mounts no `/data-store` (`vicebuild/deployments.go`: the CSI path needs `UseCSIDriver && spec.MountDataStore`); inputs are staged into the working directory by the file-transfers sidecar, and the working directory, `scratch` included, is copied to the output folder when the analysis ends. | The app needs the option on, and says so: `de/app-docs.md`, and the panel when no Data Store root exists. Staged inputs are not reachable (the working directory is not a root). |
