# Security model

This describes what `mesa-rustgis-app` defends against, how, and what it does not defend against.
The mechanisms are specified in [`architecture.md`](architecture.md) (sections 2 to 6); this
document gives the reasons. It covers milestones M1 and M3. Items that depend on Discovery
Environment (DE) behaviour not yet measured are marked **unverified** and refer to the `U`
numbers in `architecture.md` section 9.

## 1. Setting

- One process, `rgis-host`, runs as uid 1000 in the analysis container and listens on port 8080.
  It holds no credentials. It reaches the Data Store only through the directory the DE's iRODS CSI
  driver mounts at `/data-store`, with whatever permissions that mount gives the analysis owner.
- The browser reaches it only through vice-proxy, a sidecar that performs the CyVerse (Keycloak)
  login and keeps a session cookie bound to the analysis host, `https://a<8 hex>.cyverse.run`.
  vice-proxy admits the analysis owner and the users the owner has shared the analysis with.
- `rgis-host` has **no authentication and no notion of a user**. vice-proxy forwards no identity.
  `IPLANT_USER` only selects which home directory is shown; it is not a credential.
- Everything is served from one origin: the RustGIS app, the overlay, the API and the files.

What is worth protecting: the files the owner can read under the five roots (home, community
data, analysis inputs and outputs, scratch), and the integrity of the files the owner can write
(home, outputs, scratch).

## 2. Who can do what

| Actor | Can do | By design? |
|---|---|---|
| Analysis owner, in the app's page | Read every regular file under the five roots; create folders and upload (including replacing files) in the writable roots. | Yes. |
| User the analysis is shared with | Exactly the same as the owner. | Accepted, see 3.2. |
| Page on another analysis host (`*.cyverse.run`), open in the owner's browser | Nothing on `/fs` or `/api`. Can link to or navigate to the app shell. | Defended, see 3.1. |
| Page on an unrelated site, open in the owner's browser | The same: nothing on `/fs` or `/api`. | Defended, see 3.1. |
| Page on a host name re-pointed at the app's address (DNS rebinding), open in the user's browser | Nothing on `/fs` or `/api`. | Defended, see 3.1 (rule G0). |
| Someone without a DE session | Nothing: vice-proxy redirects to the login before the request reaches the app. | Relies on vice-proxy. |
| Someone who can reach port 8080 directly | Everything the owner can. | Not defended, see 4. |

## 3. Threats and defences

### 3.1 Cross-origin reads and writes from sibling analyses

**Threat.** vice-proxy wraps every analysis in a CORS policy that allows credentialed requests
from `*.cyverse.run` and `*.cyverse.org` (read from its source). Its session cookie is host-only
and `SameSite=Lax`, and every other analysis is same-site with this one. So a page served by any
other analysis, whether a notebook someone else controls or a web app with an injection flaw, can
send `fetch("https://<victim analysis>/fs/…", {credentials: "include"})` from the victim's
browser. The cookie is attached, vice-proxy lets the request through and adds CORS headers to the
answer, and the attacker's script reads the file. The attacker needs the victim's analysis host
name and the victim's browser on the attacker's page while the victim's session is live.

A defence that guards only mutations, or refuses only `Sec-Fetch-Site: cross-site`, misses this:
the attack is a plain `GET`, and it arrives as `same-site`.

**Defence.** The guard (`architecture.md` section 4) runs on every `/fs` and `/api` request,
`GET` and `HEAD` included, before routing:

- G0 refuses a request whose `Host` is not the app's own: the host of the public URL
  (`RGIS_PUBLIC_URL`, by default the DE's `REDIRECT_URL`), an `RGIS_ALLOWED_HOSTS` entry, or a
  loopback name (`localhost`, `127.0.0.1`, `[::1]`). See "DNS rebinding" below.
- G1 refuses a request whose `Sec-Fetch-Site` is present and is not `same-origin` or `none`.
  Browsers set this header themselves and page script cannot change it. `same-site` and
  `cross-site` are both refused.
- G2 refuses a request whose `Origin` is present and is `null`, unparsable, has another scheme
  than the app's public one, or names a host other than the one the request was sent to (plus
  the configured public host and any `RGIS_ALLOWED_HOSTS`). Browsers send `Origin` on every
  cross-origin CORS request. The scheme matters where G1 cannot help (a browser or gateway that
  drops `Sec-Fetch-Site`): the plain-`http` page on the analysis's host name is another origin,
  and one a network attacker can supply.
- Mutations additionally need the header `X-RGIS-Host: 1` (G3) and a content type no HTML form can
  send (G4). A cross-origin page cannot add that header without a preflight. This is a second
  layer only: G1 and G2 already refuse the request whatever the preflight's outcome.
- `rgis-host` never sends an `Access-Control-*` header and never approves a preflight: an
  `OPTIONS` gets 405, or 403 when G1 or G2 already refuses it.

**DNS rebinding.** G1 and G2 compare what the browser reports about the page with the `Host`
the request was sent to. A page served from a name its author controls, whose DNS answer is then
switched to the app's address (`127.0.0.1` for a local run), is same-origin to the browser:
`Sec-Fetch-Site: same-origin`, and an `Origin` equal to the `Host`. Only the `Host` itself gives
it away, and G0 checks it against names fixed in the configuration. In the DE vice-proxy's
host-bound session cookie already stops such a page; on a workstation, where the port is bound
to `127.0.0.1` and there is no login, G0 is the only thing that does. A consequence: started
without a public URL, the API answers on loopback names only.

Requests that are not readable by the sender are covered too. A sibling page that embeds a file
URL in an `<img>`, `<script>` or `<iframe>`, or navigates to it, is refused by G1 in any browser
that sends `Sec-Fetch-Site`. In one that does not, the response is still opaque to the page, and
`Cross-Origin-Resource-Policy: same-origin`, `X-Content-Type-Options: nosniff`,
`Content-Disposition: attachment` and the `frame-ancestors 'none'` policy stop it being embedded
or framed. Read routes have no side effects.

**Limits.**

- A request with neither `Sec-Fetch-Site` nor `Origin` passes G1 and G2. That is deliberate: the
  kubelet probe, command-line clients and old browsers on same-origin `GET`s send neither. A
  current browser always sends `Sec-Fetch-Site` to an `https` origin.
- **Unverified (U1, U2):** the guard is only as good as the headers that reach it. If something
  between the browser and `rgis-host` strips `Sec-Fetch-Site`, G2 still refuses CORS requests. If
  it strips both, cross-origin `GET`s from sibling analyses are **not** refused. If it rewrites
  `Host`, or `REDIRECT_URL` does not name the host and scheme the browser uses, legitimate
  requests are refused (G0, or G2 for writes) until `RGIS_PUBLIC_URL` or `RGIS_ALLOWED_HOSTS` is
  set; the log names each refused `Host`. The M2 checklist tests all of these, including an
  actual sibling-analysis request.
- The static shell (`/`, the bundle, `/host/*`) is not guarded. It contains nothing private.

### 3.2 Users the analysis is shared with act as the owner

**Threat.** The DE lets an owner share a running analysis. vice-proxy then admits those users to
the same origin and forwards nothing that tells `rgis-host` who is asking. The mount carries the
owner's Data Store permissions. A shared-with user can therefore read everything the owner can
under the five roots, including the owner's whole home directory, and can create folders and
upload into the owner's home and outputs, replacing files if they confirm the prompt.

**Defence.** None in the default app: the service cannot tell users apart. What limits the damage:

- There is no delete, rename or move route (3.6).
- Files and folders whose names start with `.` cannot be listed, read or written, directly or
  through a symlink (3.3), which keeps the usual credential and configuration files in a home
  directory out of reach.
- `RGIS_READONLY=1` (rule G5) refuses every mutation before any file-system call. A second,
  read-only DE app that sets it is planned for analyses meant to be shared. Read access is
  unchanged in that variant: a shared-with user still reads everything the owner can.

**What to tell users.** Sharing a MESA RustGIS analysis shares read access to your Data Store
home, and write access unless it is the read-only app. Share it only with people you would give
that access. The exact behaviour of sharing in the DE is on the M2 checklist.

### 3.3 Path confinement

**Threat.** A request names a path that leaves the intended directories: `..`, an absolute path,
an encoded separator, a symlink to somewhere else on the container or the mount, a device or
FIFO, or another user's home under `/iplant/home`.

**Defence.**

- A route can name a file only by a virtual path under one of five fixed roots. The only entries
  under `/<zone>/home` that are roots are the owner's own directory and `shared`.
- Syntax is checked before any file-system call: no `..`, `.` or empty segment, no NUL, backslash
  or control byte, no segment starting with `.`, no `.irodssymlink` suffix, bounded lengths.
- Resolution happens inside one `os.Root` per root, so the lookup itself cannot leave the root's
  directory, whatever symlinks exist and whenever they are created.
- No symlink is followed, including one that stays inside the root. The syntax rules see only
  the names in the request: `key.txt -> .ssh/id_rsa` or `d -> .ssh` would otherwise hand out, or
  accept an upload into, exactly the dot-named entries the rules exist to protect. Every
  component below the root is checked with `lstat`; a path through a link answers 404, links are
  not listed, and `mkdir` and upload treat a link at the final name as taken (409). The service
  cannot create symlinks, but the mount can hold them (irodsfs supports them, so any other tool
  with the same Data Store may have made one).
- Only regular files are served and only regular files and directories are listed.
- The user name is validated (`^[A-Za-z0-9._@-]{1,64}$`, not starting with `.`, not `shared`)
  before it is used as a path segment. An invalid value drops the home root; it never widens it.
- The application directory (`/opt/rustgis/app`) is not a root, so nothing uploaded can become
  part of the served app. The scratch root is a subdirectory of the working directory, not the
  working directory itself, which may hold links into the Data Store.
- Upload temp files are named `.rgis-upload-<32 hex>.part` in the destination directory; the id is
  validated, and the abort route can remove only a file of that name.

**Limits.** Confinement is to the root directories, not to an iRODS permission model: whatever the
mount exposes under a root is reachable. Whether the community-data mount is itself read-only is
unverified (U10); the service refuses writes there regardless. The symlink check and the open
that follows are separate system calls: someone who can create and swap symlinks inside a root
while a request runs can get one followed. `os.Root` still keeps that inside the root, and such
a writer has the Data Store's own access to the files already.

### 3.4 Stored cross-site scripting through `/fs`

**Threat.** Files share the app's origin. If a file from the Data Store were ever rendered as a
document, script in it would run with full access to `/api` and `/fs`: it could read and overwrite
the owner's files. The file could come from community data, from a collaborator's upload, or from
the owner's own downloads. HTML and SVG are the obvious carriers; content sniffing makes others.

**Defence.** `/fs` responses are never renderable:

- `Content-Type` comes from a fixed table by extension. No HTML, SVG or XML type is ever sent;
  unknown extensions are `application/octet-stream`. The type is set before `http.ServeContent`,
  so nothing is sniffed server-side, and `X-Content-Type-Options: nosniff` stops the browser.
- `Content-Disposition: attachment` makes a navigation to a file URL a download.
- `Content-Security-Policy: sandbox; default-src 'none'; frame-ancestors 'none'` means that even
  if a browser did render the response, it would be in an opaque origin with no script and no
  subresources.
- The project route (`/api/v1/project/…`) returns `application/json` with the same headers.
- `/api` responses are JSON with the same policy.

In the overlay, file and folder names are untrusted data. The DOM is built with `createElement`
and `textContent`; names are never passed to `innerHTML`. URLs are built by encoding each path
segment.

**Limits.** This protects the app's origin from file *content served by `rgis-host`*. It does not
make the files safe for RustGIS to parse (see 4).

### 3.5 Clickjacking

**Threat.** Another site frames the app and tricks the user into clicking Upload, confirming a
replace, or opening attacker-chosen data.

**Defence.** The shell is sent with `Content-Security-Policy: frame-ancestors 'self'`; `/fs` and
`/api` with `frame-ancestors 'none'`. The app cannot be framed by another origin.

**Consequence.** The app cannot be embedded in the MESA portal or any other page. The portal can
link to it.

### 3.6 Destructive actions

**Threat.** A bug, a confused user or a shared-with user destroys data.

**Defence.**

- There is no delete, rename, move or copy route. `DELETE /api/v1/upload` removes only an upload's
  own temp file.
- `mkdir` creates one directory and fails if the name exists.
- An upload replaces an existing file only with `overwrite=1`, which the panel sends only after
  the user confirms. Without it the answer is `409 exists`. A directory or a symlink is never
  replaced, and the server never removes an existing entry itself: the only way a file goes is
  `rename(2)` over it.
- The final name is checked twice: before the body is read, and again at commit, under a lock on
  that name. A body can take minutes to arrive, so the first check alone would let a folder
  created meanwhile be replaced by an `overwrite=1` upload, or let two uploads of a new name
  both succeed. Without `overwrite` the commit is a hard link where the filesystem has them,
  which cannot replace anything.
- An upload is written to a temp file and renamed on commit, so a failed or abandoned upload
  never leaves a truncated file under the final name. `Sync` and `Close` are checked before the
  server reports a chunk as stored, and the commit checks the total size.
- `RGIS_READONLY=1` turns all of this off.

**Limits.**

- Replacing a file is permanent; there is no versioning or trash.
- On the Data Store mount (irodsfs has no hard links) a commit without `overwrite` is a check
  followed by a rename. Uploads through this service cannot race each other there (the lock),
  but a file of the same name created by something else, such as the DE's Data window, in the
  instant between the two is replaced.
- A filesystem that refused to rename over an existing file would make every replace fail; the
  server does not remove the old file to get round it. irodsfs renames over a file (U5).
- Abandoned temp files are not cleaned up. They are hidden from listings and count against quota.
- By default there is no upload size limit (`RGIS_MAX_UPLOAD=0`); the Data Store quota is the
  limit.

### 3.7 Redirects and session expiry

`rgis-host` never redirects, so it can never send the browser, or a credentialed fetch, to another
origin. When the DE session expires, vice-proxy answers with a redirect to the login. The overlay
sends every request with `redirect: "manual"`, treats the resulting opaque redirect as "session
expired", stops, and never follows it or re-sends an upload body anywhere else.

### 3.8 Information in logs and errors

Request logs hold method, path, status, size and duration: no headers, no cookies, no query
strings. The one header value ever logged is a `Host` that rule G0 refused. The paths of files read through `/fs` and of projects do appear (they are in the URL
path); paths passed to `list`, `stat`, `mkdir` and `upload` do not. Error bodies carry a fixed
code and a short message, which must not include on-disk paths.

### 3.9 The image

- No shell tools beyond the base image, no package manager use at run time, no agent CLIs, no
  iRODS client, no credentials, no secrets in layers.
- The upstream bundle is fetched by release URL and checked against a SHA-256 pinned in
  `UPSTREAM_REF`. `rgis-host` uses the Go standard library only, so there is no module download.
- The server makes no outbound network request. Its only client use is the health check, to
  `127.0.0.1`.
- The base images are pinned by digest, so a rebuild does not change them; dependabot proposes
  new digests as pull requests.
- CI lints the Dockerfile and scans the published images, `:latest` and the version tag DE tools
  pin (`security.yml`).

## 4. What is not defended

- **Direct access to the port.** `rgis-host` trusts that vice-proxy is the only way in. Run
  outside the DE with the port published on a reachable interface, it gives unauthenticated read
  and write access to the mounted directories to anyone who can connect (a client that is not a
  browser sends whatever `Host` it likes, so G0 is no defence there). Bind it to `127.0.0.1` for
  local use; G0 then covers the one remaining route in, a web page in the local browser (3.1).
- **The owner, and anyone the owner shares with.** They are the same principal to this service
  (3.2). There is no per-user access control, audit trail or rate limit.
- **Compromise of the app's own origin.** Script running in the app's page can do everything the
  user can. That includes a flaw in the RustGIS bundle, in the overlay, or a hostile browser
  extension. In particular, RustGIS parses the files it opens inside WebAssembly; a file crafted
  to exploit a parser bug there is upstream's attack surface, and this image does not reduce it.
- **Hostile launch links.** The shell accepts `?data=<url>` and `?project=<url>` from anyone who
  can get the user to follow a link to their analysis, and RustGIS will then fetch and open the
  attacker's data or project. This is upstream behaviour and is how "Open in new map" works; it
  cannot be closed without breaking it. It gives the attacker no read access by itself.
- **The upstream supply chain.** The checksum pins which release is shipped; it does not vouch
  for what is in it. The bundle's service worker and its third-party code are trusted as shipped.
- **Outbound requests from the browser.** RustGIS loads basemaps and whatever URLs a project
  names, and at start-up the v0.6.0 bundle itself fetches from `assets.rustgis.app` (icons) and
  `plugins.rustgis.app` (seen in the local browser pass). Nothing here restricts that; the
  shell's CSP is `frame-ancestors` only, because a stricter policy would have to be kept in
  step with upstream.
- **Transport.** TLS ends before vice-proxy; `rgis-host` speaks plain HTTP inside the pod.
- **Denial of service.** No request rate limits; listing reads and sorts a whole directory per
  call. The project route holds the file in memory: one read-and-rewrite at a time, up to twice
  `RGIS_MAX_PROJECT` while it runs, and each response in flight keeps its buffer until the
  client has taken it or the write deadline passes (30 s plus 1 s per 64 KiB). Several slow
  readers of a large project can therefore hold several buffers at once. Only same-origin pages
  reach the route, and the container's CPU and memory limits are the bound.
- **Data Store semantics.** Consistency, caching and permission behaviour of the CSI mount are
  taken as found (U4, U5, U10, U11).

## 5. Assumptions to confirm in the DE

These decide whether section 3.1 holds as written. Each is a row in the M2 table in
[`verification.md`](verification.md).

| Assumption | If false |
|---|---|
| `Sec-Fetch-Site` and `Origin` reach `rgis-host` unchanged (U2). | Sibling-analysis `GET`s may not be refused. Do not publish until fixed. |
| `Host` reaches `rgis-host` as the browser sent it, and equals the host of `REDIRECT_URL`; its scheme is the browser's (U1, U2). | Legitimate requests are refused (G0; G2 for writes); set `RGIS_PUBLIC_URL` or `RGIS_ALLOWED_HOSTS`. |
| A request from a second analysis to `/fs` and `/api` is answered 403. | As the first row. |
| vice-proxy is the only route to port 8080. | Section 4, first item, applies inside the DE. |
| A shared-with user reaches the app as described in 3.2, and no more. | Revise 3.2 and the user-facing warning. |

## 6. Reporting

Report a suspected vulnerability privately to the repository maintainers through GitHub's
"Report a vulnerability" (Security tab) rather than in a public issue. Flaws in RustGIS itself
belong with [opengeos/RustGIS](https://github.com/opengeos/RustGIS).
