// Copyright The MESA RustGIS App authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"syscall"
	"testing"
)

const lockedCSPWant = "sandbox; default-src 'none'; frame-ancestors 'none'"

func TestFSGetAndHeaders(t *testing.T) {
	e := newEnv(t)
	w := e.get("/fs"+home+"/dem.tif", "Accept-Encoding", "gzip")
	wantStatus(t, w, 200)
	if w.Body.String() != string(demBytes) {
		t.Errorf("body %q", w.Body.String())
	}
	for k, want := range map[string]string{
		"Content-Type":                 "image/tiff",
		"Content-Length":               fmt.Sprint(len(demBytes)),
		"Accept-Ranges":                "bytes",
		"Content-Disposition":          "attachment",
		"Content-Security-Policy":      lockedCSPWant,
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cache-Control":                "private, no-cache",
		"X-Content-Type-Options":       "nosniff",
		"Content-Encoding":             "",
		"Access-Control-Allow-Origin":  "",
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if w.Header().Get("Last-Modified") == "" {
		t.Error("Last-Modified missing")
	}
	fi, _ := os.Stat(e.ds + home + "/dem.tif")
	if got, want := w.Header().Get("ETag"), fmt.Sprintf(`"%x-%x"`, fi.ModTime().UnixNano(), fi.Size()); got != want {
		t.Errorf("ETag = %s, want %s", got, want)
	}
}

func TestFSRange(t *testing.T) {
	e := newEnv(t)
	target := "/fs" + home + "/dem.tif"
	n := len(demBytes)
	for _, tc := range []struct{ rng, body, contentRange string }{
		{"bytes=2-5", "2345", fmt.Sprintf("bytes 2-5/%d", n)},
		{"bytes=30-", "uvwxyz", fmt.Sprintf("bytes 30-35/%d", n)},
		{"bytes=-3", "xyz", fmt.Sprintf("bytes 33-35/%d", n)},
		{"bytes=0-0", "0", fmt.Sprintf("bytes 0-0/%d", n)},
	} {
		w := e.get(target, "Range", tc.rng)
		wantStatus(t, w, 206)
		if w.Body.String() != tc.body || w.Header().Get("Content-Range") != tc.contentRange {
			t.Errorf("%s: body %q, Content-Range %q", tc.rng, w.Body.String(), w.Header().Get("Content-Range"))
		}
		if w.Header().Get("Content-Type") != "image/tiff" {
			t.Errorf("%s: Content-Type %q", tc.rng, w.Header().Get("Content-Type"))
		}
	}
	w := e.get(target, "Range", "bytes=1000-")
	wantStatus(t, w, 416)
	if got := w.Header().Get("Content-Range"); got != fmt.Sprintf("bytes */%d", n) {
		t.Errorf("416 Content-Range = %q", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("416 Content-Type = %q", ct)
	}

	etag := e.get(target).Header().Get("ETag")
	wantStatus(t, e.get(target, "If-None-Match", etag), 304)
	wantStatus(t, e.get(target, "Range", "bytes=0-1", "If-Range", etag), 206)
	wantStatus(t, e.get(target, "Range", "bytes=0-1", "If-Range", `"stale"`), 200)
}

func TestFSHead(t *testing.T) {
	e := newEnv(t)
	w := e.do("HEAD", "/fs"+home+"/dem.tif", nil)
	wantStatus(t, w, 200)
	if w.Body.Len() != 0 {
		t.Errorf("HEAD body %q", w.Body.String())
	}
	h := w.Header()
	if h.Get("Accept-Ranges") != "bytes" || h.Get("Content-Length") != fmt.Sprint(len(demBytes)) ||
		h.Get("Content-Type") != "image/tiff" || h.Get("ETag") == "" || h.Get("Last-Modified") == "" {
		t.Errorf("HEAD headers %v", h)
	}
	w = e.do("HEAD", "/fs"+home+"/missing.tif", nil)
	wantStatus(t, w, 404)
	if w.Body.Len() != 0 {
		t.Errorf("HEAD error body %q", w.Body.String())
	}
	if w.Header().Get("Content-Disposition") != "attachment" {
		t.Error("error response lost its class headers")
	}
}

func TestFSContentTypes(t *testing.T) {
	e := newEnv(t)
	for name, want := range map[string]string{
		"a.tif": "image/tiff", "a.TIFF": "image/tiff", "a.json": "application/json",
		"a.geojson": "application/json", "a.rgis": "application/json",
		"a.csv": "text/csv; charset=utf-8", "a.txt": "text/plain; charset=utf-8",
		"a.png": "image/png", "a.jpg": "image/jpeg", "a.JPEG": "image/jpeg",
		"a.html": "application/octet-stream", "a.svg": "application/octet-stream",
		"a.xml": "application/octet-stream", "a.js": "application/octet-stream",
		"noext": "application/octet-stream", "a.fgb": "application/octet-stream",
	} {
		// Content that net/http would sniff as HTML.
		e.write(e.ds+home+"/"+name, []byte("<html><script>alert(1)</script></html>"))
		w := e.get("/fs" + home + "/" + name)
		wantStatus(t, w, 200)
		if got := w.Header().Get("Content-Type"); got != want {
			t.Errorf("%s: Content-Type %q, want %q", name, got, want)
		}
	}
}

func TestFSNotFoundAndMethods(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{
		"/fs", "/fs/", "/fs" + home, "/fs" + home + "/sub", "/fs" + home + "/missing",
		"/fs" + home + "/notes.txt/x", "/fs/iplant", "/fs/iplant/home", "/fs/iplant/home/bob/private.txt",
		"/fs/iplant/home/alicex/dem.tif", "/fs/nowhere/x", "/fs/input/x",
	} {
		wantErr(t, e.get(p), 404, "not_found")
	}
	wantErr(t, e.get("/fs"+home+"/"), 400, "bad_path")
	wantErr(t, e.get("/fs/"+home+"/dem.tif"), 400, "bad_path")
	for _, m := range []string{"POST", "PUT", "DELETE", "OPTIONS", "PATCH"} {
		w := e.do(m, "/fs"+home+"/dem.tif", nil)
		wantErr(t, w, 405, "method_not_allowed")
		if w.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: Allow %q", m, w.Header().Get("Allow"))
		}
	}
	if string(mustRead(t, e.ds+home+"/dem.tif")) != string(demBytes) {
		t.Error("file changed by a refused method")
	}
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTraversal(t *testing.T) {
	e := newEnv(t)
	e.write(e.ds+"/top.txt", []byte("top"))
	for _, p := range []string{
		home + "/../bob/private.txt",
		home + "/sub/../../bob/private.txt",
		home + "/%2e%2e/bob/private.txt",
		home + "/%2E%2E/bob/private.txt",
		home + "/..%2fbob%2fprivate.txt",
		home + "/sub%2f..%2f..%2fbob/private.txt",
		home + "/./dem.tif",
		home + "/%2e/dem.tif",
		home + "//dem.tif",
		home + "/dem.tif%00.png",
		home + "/%00",
		home + "/sub%5c..%5cdem.tif",
		home + "/..%5c..%5ctop.txt",
		home + "/dem.tif%0a",
		"/..",
		"/../top.txt",
		"/%2e%2e/%2e%2e/etc/passwd",
		home + "/%ff",
	} {
		for _, prefix := range []string{"/fs", "/api/v1/project"} {
			w := e.get(prefix + p)
			wantErr(t, w, 400, "bad_path")
			if strings.Contains(w.Body.String(), "bob\n") {
				t.Errorf("%s leaked a file", p)
			}
		}
	}
	for _, p := range []string{
		home + "/../bob", home + "/..", "/", "iplant/home/alice", home + "/", home + "//sub",
		home + "/a\x00b", home + "\\sub", home + "/sub\\..", "/iplant/home/alice/./sub", "..",
	} {
		wantErr(t, e.get("/api/v1/list"+q("path", p)), 400, "bad_path")
		wantErr(t, e.get("/api/v1/stat"+q("path", p)), 400, "bad_path")
		wantErr(t, e.mkdir(p+"/new"), 400, "bad_path")
		wantErr(t, e.put(q("path", p+"/up.bin", "id", testID, "offset", "0"), []byte("x")), 400, "bad_path")
	}
	wantErr(t, e.get("/api/v1/list"+q("path", "")), 400, "bad_path")
}

func TestSymlinks(t *testing.T) {
	e := newEnv(t)
	links := map[string]string{
		"abs-file":     e.outside + "/secret.txt",
		"abs-dir":      e.outside,
		"abs-inside":   e.ds + home + "/dem.tif",
		"rel-escape":   "../../../../outside/secret.txt",
		"to-bob":       "../bob/private.txt",
		"to-shared":    "../shared",
		"dangling":     "nothing-here",
		"loop":         "loop",
		"inside.tif":   "dem.tif",
		"inside-dir":   "sub",
		"sub/back.tif": "../dem.tif",
	}
	for name, target := range links {
		if err := os.Symlink(target, e.ds+home+"/"+name); err != nil {
			t.Fatal(err)
		}
	}
	// No symlink is followed, the ones that stay inside the root included.
	for _, name := range []string{
		"abs-file", "abs-inside", "rel-escape", "to-bob", "dangling", "loop", "abs-dir/secret.txt", "to-shared/pub.txt",
		"inside.tif", "sub/back.tif", "inside-dir/inner.csv", "inside-dir/back.tif",
	} {
		w := e.get("/fs" + home + "/" + name)
		wantErr(t, w, 404, "not_found")
		wantErr(t, e.get("/api/v1/stat"+q("path", home+"/"+name)), 404, "not_found")
	}
	for _, name := range []string{"abs-dir", "to-shared", "inside-dir"} {
		wantErr(t, e.get("/api/v1/list"+q("path", home+"/"+name)), 404, "not_found")
		wantErr(t, e.get("/api/v1/stat"+q("path", home+"/"+name)), 404, "not_found")
		wantErr(t, e.mkdir(home+"/"+name+"/new"), 404, "not_found")
		wantErr(t, e.put(q("path", home+"/"+name+"/up.bin", "id", testID, "offset", "0", "commit", "1", "size", "1"), []byte("x")), 404, "not_found")
		wantStatus(t, e.do("HEAD", "/api/v1/upload"+q("path", home+"/"+name+"/up.bin", "id", testID), nil), 404)
		wantErr(t, e.get("/api/v1/project"+home+"/"+name+"/p.rgis"), 404, "not_found")
	}
	// A link occupies its name: nothing is created through or over it.
	wantErr(t, e.mkdir(home+"/dangling"), 409, "exists")
	wantErr(t, e.mkdir(home+"/inside-dir"), 409, "exists")
	if entries, _ := os.ReadDir(e.outside); len(entries) != 1 {
		t.Errorf("something was written outside the root: %v", entries)
	}
	if entries, _ := os.ReadDir(e.ds + home + "/sub"); len(entries) != 2 {
		t.Errorf("something was written through a link to a directory: %v", entries)
	}

	got := listNames(t, e, home)
	want := "sub dem.tif notes.txt"
	if strings.Join(got, " ") != want {
		t.Errorf("listing = %q, want %q", got, want)
	}
	if got := listNames(t, e, home+"/sub"); strings.Join(got, " ") != "inner.csv" {
		t.Errorf("listing of sub = %q", got)
	}
}

// The path rules see only the names in the request; a link that was followed
// would take a request past them, to a dot-file or into a dot-directory.
func TestSymlinkToHiddenName(t *testing.T) {
	e := newEnv(t)
	ssh := e.ds + home + "/.ssh"
	if err := os.Mkdir(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	e.write(ssh+"/id_rsa", []byte("SECRET-KEY"))
	for name, target := range map[string]string{"key.txt": ".ssh/id_rsa", "sshdir": ".ssh", "sub/up": "../.ssh"} {
		if err := os.Symlink(target, e.ds+home+"/"+name); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"key.txt", "sshdir/id_rsa", "sub/up/id_rsa"} {
		w := e.get("/fs" + home + "/" + name)
		wantErr(t, w, 404, "not_found")
		if strings.Contains(w.Body.String(), "SECRET") {
			t.Errorf("%s: the dot-file was served", name)
		}
		wantErr(t, e.get("/api/v1/stat"+q("path", home+"/"+name)), 404, "not_found")
	}
	for _, dir := range []string{"sshdir", "sub/up"} {
		wantErr(t, e.get("/api/v1/list"+q("path", home+"/"+dir)), 404, "not_found")
		wantErr(t, e.mkdir(home+"/"+dir+"/new"), 404, "not_found")
		args := []string{"path", home + "/" + dir + "/authorized_keys", "id", testID, "offset", "0", "commit", "1", "size", "1"}
		wantErr(t, e.put(q(args...), []byte("x")), 404, "not_found")
		wantErr(t, e.put(q(append(args, "overwrite", "1")...), []byte("x")), 404, "not_found")
		wantErr(t, e.do("DELETE", "/api/v1/upload"+q("path", home+"/"+dir+"/authorized_keys", "id", testID), nil, "X-RGIS-Host", "1"), 404, "not_found")
	}
	if entries, _ := os.ReadDir(ssh); len(entries) != 1 {
		t.Errorf("something was written into the dot-directory: %v", entries)
	}
	if got := strings.Join(listNames(t, e, home), " "); got != "sub dem.tif notes.txt" {
		t.Errorf("listing = %q", got)
	}
}

type listBody struct {
	Path       string  `json:"path"`
	Root       string  `json:"root"`
	Writable   bool    `json:"writable"`
	Entries    []entry `json:"entries"`
	NextCursor string  `json:"nextCursor"`
}

func listNames(t *testing.T, e *env, vpath string) []string {
	t.Helper()
	w := e.get("/api/v1/list" + q("path", vpath))
	wantStatus(t, w, 200)
	names := []string{}
	for _, en := range decode[listBody](t, w).Entries {
		names = append(names, en.Name)
	}
	return names
}

func TestHiddenNames(t *testing.T) {
	e := newEnv(t)
	os.Mkdir(e.ds+home+"/.git", 0o755)
	e.write(e.ds+home+"/.git/config", []byte("x"))
	e.write(e.ds+home+"/.hidden", []byte("x"))
	e.write(e.ds+home+"/.rgis-upload-"+testID+".part", []byte("x"))
	e.write(e.ds+home+"/link.irodssymlink", []byte("x"))
	e.write(e.ds+home+"/LINK.IrodsSymlink", []byte("x"))
	os.Mkdir(e.ds+home+"/dir.irodssymlink", 0o755)
	e.write(e.ds+home+"/dir.irodssymlink/f.txt", []byte("x"))
	e.write(e.ds+home+"/bad\\name", []byte("x"))

	for _, name := range []string{
		".hidden", ".git", ".git/config", ".rgis-upload-" + testID + ".part",
		"link.irodssymlink", "LINK.IrodsSymlink", "dir.irodssymlink/f.txt", "%2ehidden",
	} {
		wantErr(t, e.get("/fs"+home+"/"+name), 400, "bad_path")
	}
	for _, name := range []string{".hidden", ".git", "link.irodssymlink", "dir.irodssymlink", "sub/.x", "sub/x.irodssymlink"} {
		p := home + "/" + name
		wantErr(t, e.get("/api/v1/stat"+q("path", p)), 400, "bad_path")
		wantErr(t, e.get("/api/v1/list"+q("path", p)), 400, "bad_path")
		wantErr(t, e.mkdir(p), 400, "bad_path")
		wantErr(t, e.put(q("path", p, "id", testID, "offset", "0"), nil), 400, "bad_path")
	}
	if got := strings.Join(listNames(t, e, home), " "); got != "sub dem.tif notes.txt" {
		t.Errorf("listing = %q", got)
	}
}

func TestNonRegularFiles(t *testing.T) {
	e := newEnv(t)
	if err := syscall.Mkfifo(e.ds+home+"/pipe", 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	os.Symlink("pipe", e.ds+home+"/pipe-link")
	for _, name := range []string{"pipe", "pipe-link"} {
		wantErr(t, e.get("/fs"+home+"/"+name), 404, "not_found")
		wantErr(t, e.get("/api/v1/stat"+q("path", home+"/"+name)), 404, "not_found")
		wantErr(t, e.get("/api/v1/list"+q("path", home+"/"+name)), 404, "not_found")
	}
	wantErr(t, e.put(q("path", home+"/pipe", "id", testID, "offset", "0", "overwrite", "1"), nil), 409, "exists")
	if got := strings.Join(listNames(t, e, home), " "); got != "sub dem.tif notes.txt" {
		t.Errorf("listing = %q", got)
	}
}

type sessionBody struct {
	User     string `json:"user"`
	Origin   string `json:"origin"`
	ReadOnly bool   `json:"readOnly"`
	Mode     string `json:"mode"`
	Roots    []struct {
		ID, Label, Path  string
		Writable, Exists bool
	} `json:"roots"`
	Limits   map[string]int64 `json:"limits"`
	Versions map[string]any   `json:"versions"`
}

func TestSession(t *testing.T) {
	e := newEnv(t, "RGIS_VERSION", "0.6.0-1", "RGIS_UPSTREAM", "v0.6.0")
	os.Remove(e.ds + "/input")
	w := e.get("/api/v1/session")
	wantStatus(t, w, 200)
	for k, want := range map[string]string{
		"Content-Type":                 "application/json",
		"Content-Security-Policy":      lockedCSPWant,
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cache-Control":                "no-store",
		"X-Content-Type-Options":       "nosniff",
		"Content-Disposition":          "",
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	s := decode[sessionBody](t, w)
	if s.User != "alice" || s.Origin != "http://"+testHost || s.ReadOnly || s.Mode != "launch" {
		t.Errorf("session %+v", s)
	}
	var got []string
	for _, r := range s.Roots {
		got = append(got, fmt.Sprintf("%s|%s|%s|%v|%v", r.ID, r.Label, r.Path, r.Writable, r.Exists))
	}
	want := []string{
		"home|Home|/iplant/home/alice|true|true",
		"shared|Community Data|/iplant/home/shared|false|true",
		"input|Analysis Inputs|/input|false|false",
		"output|Analysis Outputs|/output|true|true",
		"scratch|Session Scratch|/scratch|true|true",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("roots:\n%s", strings.Join(got, "\n"))
	}
	if s.Limits["maxChunk"] != 8388608 || s.Limits["maxUpload"] != 0 || s.Limits["maxProject"] != 268435456 || s.Limits["maxOpen"] != 16 {
		t.Errorf("limits %v", s.Limits)
	}
	if s.Versions["api"] != float64(1) || s.Versions["host"] != "0.6.0-1" || s.Versions["upstream"] != "v0.6.0" {
		t.Errorf("versions %v", s.Versions)
	}

	// A root that appears later works without a restart.
	wantErr(t, e.get("/api/v1/list"+q("path", "/input")), 404, "not_found")
	os.Mkdir(e.ds+"/input", 0o755)
	e.write(e.ds+"/input/in.txt", []byte("in"))
	wantStatus(t, e.get("/fs/input/in.txt"), 200)
	if s := decode[sessionBody](t, e.get("/api/v1/session")); !s.Roots[2].Exists {
		t.Error("input still reported missing")
	}
}

func TestSessionOrigin(t *testing.T) {
	origin := func(e *env, hdr ...string) string {
		return decode[sessionBody](t, e.get("/api/v1/session", hdr...)).Origin
	}
	e := newEnv(t)
	for _, tc := range []struct{ proto, want string }{
		{"https", "https://" + testHost},
		{"https, http", "https://" + testHost},
		{"http", "http://" + testHost},
		{"ftp", "http://" + testHost},
		{"HTTPS", "http://" + testHost},
	} {
		if got := origin(e, "X-Forwarded-Proto", tc.proto); got != tc.want {
			t.Errorf("X-Forwarded-Proto %q: %q", tc.proto, got)
		}
	}
	// The scheme comes from the public URL, the host always from the request.
	e = newEnv(t, "REDIRECT_URL", "https://a1b2c3d4e.cyverse.run", "RGIS_ALLOWED_HOSTS", "a99999999.cyverse.run")
	if got := origin(e, "X-Forwarded-Proto", "http", "Host", "a99999999.cyverse.run"); got != "https://a99999999.cyverse.run" {
		t.Errorf("origin %q", got)
	}
	if got := origin(e, "X-Forwarded-Proto", "http"); got != "https://"+testHost {
		t.Errorf("origin on loopback %q", got)
	}
	e = newEnv(t, "REDIRECT_URL", "https://a1b2c3d4e.cyverse.run", "RGIS_PUBLIC_URL", "")
	if got := origin(e); got != "http://"+testHost {
		t.Errorf("origin with the public URL disabled: %q", got)
	}
}

func TestNoUserNoHomeRoot(t *testing.T) {
	for _, user := range []string{"", "shared", ".alice", "a/b"} {
		e := newEnv(t, "IPLANT_USER", user)
		s := decode[sessionBody](t, e.get("/api/v1/session"))
		if s.User != "" || len(s.Roots) != 4 || s.Roots[0].ID != "shared" {
			t.Errorf("user %q: %+v", user, s)
		}
		wantErr(t, e.get("/fs"+home+"/dem.tif"), 404, "not_found")
		wantErr(t, e.mkdir("/iplant/home/shared/new"), 403, "read_only")
	}
}

func TestMissingDataStore(t *testing.T) {
	e := newEnv(t, "RGIS_DATA_STORE", "/nonexistent/data-store", "RGIS_SCRATCH", "/nonexistent/scratch")
	wantStatus(t, e.get("/"), 200)
	wantStatus(t, e.get("/healthz"), 200)
	for _, r := range decode[sessionBody](t, e.get("/api/v1/session")).Roots {
		if r.Exists {
			t.Errorf("root %s exists", r.ID)
		}
	}
	wantErr(t, e.get("/api/v1/list"+q("path", home)), 404, "not_found")
	wantErr(t, e.get("/fs/scratch/x"), 404, "not_found")
	wantErr(t, e.mkdir("/scratch/new"), 404, "not_found")
	wantErr(t, e.put(q("path", "/scratch/a.bin", "id", testID, "offset", "0"), nil), 404, "not_found")
}

func TestList(t *testing.T) {
	e := newEnv(t)
	dir := e.ds + home + "/order"
	os.Mkdir(dir, 0o755)
	for _, d := range []string{"Zdir", "adir", "Bdir"} {
		os.Mkdir(dir+"/"+d, 0o755)
	}
	for _, f := range []string{"b.txt", "A.txt", "a.txt", "C.txt", "10.txt", "é.txt"} {
		e.write(dir+"/"+f, []byte(f))
	}
	w := e.get("/api/v1/list" + q("path", home+"/order"))
	wantStatus(t, w, 200)
	l := decode[listBody](t, w)
	var names []string
	for _, en := range l.Entries {
		names = append(names, en.Name)
	}
	if got, want := strings.Join(names, " "), "adir Bdir Zdir 10.txt A.txt a.txt b.txt C.txt é.txt"; got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
	if l.Path != home+"/order" || l.Root != "home" || !l.Writable || l.NextCursor != "" {
		t.Errorf("list %+v", l)
	}
	if d := l.Entries[0]; d.Type != "dir" || d.Size != 0 {
		t.Errorf("dir entry %+v", d)
	}
	rfc3339 := regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`)
	if f := l.Entries[3]; f.Type != "file" || f.Size != 6 || !rfc3339.MatchString(f.MTime) {
		t.Errorf("file entry %+v", f)
	}

	// Paging walks the same order.
	var paged []string
	cursor, pages := "", 0
	for {
		l := decode[listBody](t, e.get("/api/v1/list"+q("path", home+"/order", "limit", "4", "cursor", cursor)))
		for _, en := range l.Entries {
			paged = append(paged, en.Name)
		}
		pages++
		if cursor = l.NextCursor; cursor == "" {
			break
		}
	}
	if pages != 3 || strings.Join(paged, " ") != strings.Join(names, " ") {
		t.Errorf("paged (%d pages) = %q", pages, paged)
	}
	l = decode[listBody](t, e.get("/api/v1/list"+q("path", home+"/order", "cursor", "999")))
	if len(l.Entries) != 0 || l.NextCursor != "" {
		t.Errorf("past the end: %+v", l)
	}
	if !strings.Contains(e.get("/api/v1/list"+q("path", home+"/sub/..")).Body.String(), "bad_path") {
		t.Error("dot-dot accepted")
	}

	os.Mkdir(e.ds+home+"/empty", 0o755)
	if body := e.get("/api/v1/list" + q("path", home+"/empty")).Body.String(); !strings.Contains(body, `"entries":[]`) {
		t.Errorf("empty listing %s", body)
	}
	l = decode[listBody](t, e.get("/api/v1/list"+q("path", "/iplant/home/shared")))
	if l.Root != "shared" || l.Writable || len(l.Entries) != 1 {
		t.Errorf("shared root %+v", l)
	}

	for _, bad := range []string{
		q("path", home, "limit", "0"), q("path", home, "limit", "2001"), q("path", home, "limit", "x"),
		q("path", home, "limit", "-1"), q("path", home, "limit", "+5"), q("path", home, "limit", ""),
		q("path", home, "cursor", "x"), q("path", home, "cursor", "-1"),
		"", "?path=" + home + "&path=/output", "?path=%zz",
	} {
		wantErr(t, e.get("/api/v1/list"+bad), 400, "bad_request")
	}
	wantStatus(t, e.get("/api/v1/list"+q("path", home, "limit", "2000")), 200)
	wantErr(t, e.get("/api/v1/list"+q("path", home+"/dem.tif")), 400, "not_a_directory")
	wantErr(t, e.get("/api/v1/list"+q("path", home+"/missing")), 404, "not_found")
	wantErr(t, e.get("/api/v1/list"+q("path", "/iplant/home/bob")), 404, "not_found")
	wantErr(t, e.get("/api/v1/list"+q("path", "/iplant/home")), 404, "not_found")
}

type statBody struct {
	Path     string `json:"path"`
	Root     string `json:"root"`
	Writable bool   `json:"writable"`
	Entry    entry  `json:"entry"`
}

func TestStat(t *testing.T) {
	e := newEnv(t)
	s := decode[statBody](t, e.get("/api/v1/stat"+q("path", home+"/dem.tif")))
	if s.Path != home+"/dem.tif" || s.Root != "home" || !s.Writable ||
		s.Entry.Name != "dem.tif" || s.Entry.Type != "file" || s.Entry.Size != int64(len(demBytes)) {
		t.Errorf("stat %+v", s)
	}
	s = decode[statBody](t, e.get("/api/v1/stat"+q("path", home)))
	if s.Entry.Name != "alice" || s.Entry.Type != "dir" || s.Entry.Size != 0 {
		t.Errorf("root stat %+v", s)
	}
	s = decode[statBody](t, e.get("/api/v1/stat"+q("path", "/iplant/home/shared/pub.txt")))
	if s.Root != "shared" || s.Writable {
		t.Errorf("shared stat %+v", s)
	}
	wantErr(t, e.get("/api/v1/stat"+q("path", home+"/missing")), 404, "not_found")
	wantErr(t, e.get("/api/v1/stat"+q("path", "/elsewhere")), 404, "not_found")
	wantErr(t, e.get("/api/v1/stat"), 400, "bad_request")
}

func TestMkdir(t *testing.T) {
	e := newEnv(t)
	w := e.mkdir(home + "/new folder")
	wantStatus(t, w, 201)
	m := decode[struct {
		Path  string `json:"path"`
		Entry entry  `json:"entry"`
	}](t, w)
	if m.Path != home+"/new folder" || m.Entry.Name != "new folder" || m.Entry.Type != "dir" || m.Entry.Size != 0 || m.Entry.MTime == "" {
		t.Errorf("mkdir %+v", m)
	}
	if fi, err := os.Stat(e.ds + home + "/new folder"); err != nil || !fi.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
	wantStatus(t, e.mkdir("/output/o"), 201)
	wantStatus(t, e.mkdir("/scratch/s", "Content-Type", "application/json; charset=utf-8"), 201)

	wantErr(t, e.mkdir(home+"/new folder"), 409, "exists")
	wantErr(t, e.mkdir(home+"/dem.tif"), 409, "exists")
	wantErr(t, e.mkdir(home), 409, "exists")
	wantErr(t, e.mkdir(home+"/a/b"), 404, "not_found")
	wantErr(t, e.mkdir(home+"/dem.tif/x"), 404, "not_found")
	wantErr(t, e.mkdir("/nowhere/x"), 404, "not_found")
	wantErr(t, e.mkdir("/iplant/home/bob/x"), 404, "not_found")
	wantErr(t, e.mkdir("/iplant/home/shared/x"), 403, "read_only")
	wantErr(t, e.mkdir("/input/x"), 403, "read_only")

	post := func(body string) *httptest.ResponseRecorder {
		return e.do("POST", "/api/v1/mkdir", []byte(body), "X-RGIS-Host", "1", "Content-Type", "application/json")
	}
	for _, body := range []string{``, `{}`, `[]`, `"x"`, `{"path": 5}`, `{"path": null}`, `{"path":"/output/x"} trailing`, `path=/output/x`,
		`{"path":"/output/big","pad":"` + strings.Repeat("x", 8192) + `"}`} {
		wantErr(t, post(body), 400, "bad_request")
	}
	wantStatus(t, post(`{"path":"/output/extra","other":[1,2]}`), 201)
	if _, err := os.Stat(e.ds + "/output/big"); err == nil {
		t.Error("oversized request created a directory")
	}
}

func TestReadOnlyMode(t *testing.T) {
	e := newEnv(t, "RGIS_READONLY", "TRUE")
	s := decode[sessionBody](t, e.get("/api/v1/session"))
	if !s.ReadOnly {
		t.Error("readOnly false")
	}
	for _, r := range s.Roots {
		if r.Writable {
			t.Errorf("root %s writable", r.ID)
		}
	}
	if l := decode[listBody](t, e.get("/api/v1/list"+q("path", home))); l.Writable {
		t.Error("listing writable")
	}
	wantStatus(t, e.get("/fs"+home+"/dem.tif"), 200)
	wantErr(t, e.mkdir(home+"/x"), 403, "read_only")
	wantErr(t, e.mkdir("/scratch/x"), 403, "read_only")
	up := q("path", home+"/up.bin", "id", testID, "offset", "0", "commit", "1", "size", "1")
	wantErr(t, e.put(up, []byte("x")), 403, "read_only")
	wantErr(t, e.put(q("path", home+"/dem.tif", "id", testID, "offset", "0", "commit", "1", "size", "1", "overwrite", "1"), []byte("x")), 403, "read_only")
	wantErr(t, e.do("DELETE", "/api/v1/upload"+up, nil, "X-RGIS-Host", "1"), 403, "read_only")
	wantStatus(t, e.do("HEAD", "/api/v1/upload"+up, nil), 200)
	if got := strings.Join(listNames(t, e, home), " "); got != "sub dem.tif notes.txt" {
		t.Errorf("read-only store changed: %q", got)
	}
	if string(mustRead(t, e.ds+home+"/dem.tif")) != string(demBytes) {
		t.Error("read-only file changed")
	}
}

func TestAPIRouting(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api", "/api/", "/api/v1", "/api/v2/session", "/api/v1/session/", "/api/v1/delete", "/api/v1/rename", "/api/v1/project"} {
		wantErr(t, e.get(p), 404, "not_found")
		wantErr(t, e.do("DELETE", p, nil, "X-RGIS-Host", "1"), 404, "not_found")
	}
	for _, tc := range []struct{ method, path, allow string }{
		{"POST", "/api/v1/session", "GET"},
		{"HEAD", "/api/v1/session", "GET"},
		{"DELETE", "/api/v1/list", "GET"},
		{"PUT", "/api/v1/stat", "GET"},
		{"GET", "/api/v1/mkdir", "POST"},
		{"PUT", "/api/v1/mkdir", "POST"},
		{"GET", "/api/v1/upload", "PUT, HEAD, DELETE"},
		{"POST", "/api/v1/upload", "PUT, HEAD, DELETE"},
		{"POST", "/api/v1/project" + home + "/p.rgis", "GET, HEAD"},
		{"OPTIONS", "/api/v1/session", "GET"},
		{"OPTIONS", "/api/v1/upload", "PUT, HEAD, DELETE"},
		{"OPTIONS", "/api/v1/mkdir", "POST"},
		{"OPTIONS", "/api/v1/nothing", "GET, HEAD"},
	} {
		w := e.do(tc.method, tc.path, nil, "X-RGIS-Host", "1", "Content-Type", "application/json")
		if w.Code != 405 || w.Header().Get("Allow") != tc.allow {
			t.Errorf("%s %s: %d Allow %q", tc.method, tc.path, w.Code, w.Header().Get("Allow"))
		}
		if tc.method != "HEAD" {
			wantErr(t, w, 405, "method_not_allowed")
		}
		for k := range w.Header() {
			if strings.HasPrefix(k, "Access-Control-") {
				t.Errorf("%s %s sent %s", tc.method, tc.path, k)
			}
		}
	}
	// A same-origin preflight is not approved either.
	w := e.do("OPTIONS", "/api/v1/upload", nil, "Origin", "http://"+testHost,
		"Access-Control-Request-Method", "PUT", "Access-Control-Request-Headers", "x-rgis-host")
	wantErr(t, w, 405, "method_not_allowed")
}

func TestFSErrorMapping(t *testing.T) {
	wrap := func(errno syscall.Errno) error { return &fs.PathError{Op: "open", Path: "x", Err: errno} }
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{wrap(syscall.ENOENT), 404, "not_found"},
		{wrap(syscall.ENOTDIR), 404, "not_found"},
		{wrap(syscall.ELOOP), 404, "not_found"},
		{errors.New("path escapes from parent"), 404, "not_found"},
		{wrap(syscall.EACCES), 403, "forbidden"},
		{wrap(syscall.EPERM), 403, "forbidden"},
		{wrap(syscall.EROFS), 403, "forbidden"},
		{wrap(syscall.ENOSPC), 507, "no_space"},
		{wrap(syscall.EDQUOT), 507, "no_space"},
		{wrap(syscall.EIO), 500, "io_error"},
		// irodsfs answers every refusal on the iRODS side with EREMOTEIO.
		{wrap(syscall.EREMOTEIO), 502, "store_error"},
		{&os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.EREMOTEIO}, 502, "store_error"},
	} {
		if e := fsError(tc.err); e.status != tc.status || e.code != tc.code {
			t.Errorf("fsError(%v) = %d %s, want %d %s", tc.err, e.status, e.code, tc.status, tc.code)
		}
	}
}

func TestPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not subject to file permissions")
	}
	e := newEnv(t)
	locked := e.ds + home + "/locked"
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(locked+"/a.txt", []byte("a"))
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	wantErr(t, e.mkdir(home+"/locked/new"), 403, "forbidden")
	wantErr(t, e.put(q("path", home+"/locked/b.bin", "id", testID, "offset", "0"), []byte("x")), 403, "forbidden")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	wantErr(t, e.get("/api/v1/list"+q("path", home+"/locked")), 403, "forbidden")
	wantErr(t, e.get("/fs"+home+"/locked/a.txt"), 403, "forbidden")
}
