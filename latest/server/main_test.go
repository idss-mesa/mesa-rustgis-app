// Copyright The MESA RustGIS App authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testHost = "localhost:8080"
	home     = "/iplant/home/alice"
	testID   = "0123456789abcdef0123456789abcdef"
)

var demBytes = []byte("0123456789abcdefghijklmnopqrstuvwxyz")

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// env is a fixture Data Store, bundle and scratch directory with a server on
// top. restart builds a new server over the same directories.
type env struct {
	t                         *testing.T
	ds, app, scratch, outside string
	vars                      map[string]string
	h                         http.Handler
}

func newEnv(t *testing.T, kv ...string) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{
		t:       t,
		ds:      filepath.Join(base, "ds"),
		app:     filepath.Join(base, "app"),
		scratch: filepath.Join(base, "scratch"),
		outside: filepath.Join(base, "outside"),
	}
	for _, d := range []string{
		e.ds + home + "/sub", e.ds + "/iplant/home/shared", e.ds + "/iplant/home/bob",
		e.ds + "/input", e.ds + "/output", e.app + "/host", e.scratch, e.outside,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.write(e.ds+home+"/dem.tif", demBytes)
	e.write(e.ds+home+"/notes.txt", []byte("hello\n"))
	e.write(e.ds+home+"/sub/inner.csv", []byte("a,b\n"))
	e.write(e.ds+"/iplant/home/shared/pub.txt", []byte("public\n"))
	e.write(e.ds+"/iplant/home/bob/private.txt", []byte("bob\n"))
	e.write(e.outside+"/secret.txt", []byte("secret\n"))
	e.write(e.app+"/index.html", []byte("<!doctype html><title>RustGIS</title>"))
	e.vars = map[string]string{
		"RGIS_APP_DIR":    e.app,
		"RGIS_DATA_STORE": e.ds,
		"RGIS_SCRATCH":    e.scratch,
		"IPLANT_USER":     "alice",
	}
	for i := 0; i < len(kv); i += 2 {
		e.vars[kv[i]] = kv[i+1]
	}
	e.restart()
	return e
}

func (e *env) restart() {
	e.t.Helper()
	cfg, err := loadConfig(func(k string) (string, bool) { v, ok := e.vars[k]; return v, ok })
	if err != nil {
		e.t.Fatal(err)
	}
	e.h = logRequests(newServer(cfg))
}

func (e *env) write(name string, data []byte) {
	e.t.Helper()
	if err := os.WriteFile(name, data, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// do sends one request. hdr is key, value pairs; "Host" sets the request host.
func (e *env) do(method, target string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	r.Host = testHost
	for i := 0; i < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			r.Host = hdr[i+1]
			continue
		}
		r.Header.Add(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *env) get(target string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do("GET", target, nil, hdr...)
}

func (e *env) mkdir(vpath string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	body, _ := json.Marshal(map[string]string{"path": vpath})
	return e.do("POST", "/api/v1/mkdir", body, append([]string{"X-RGIS-Host", "1", "Content-Type", "application/json"}, hdr...)...)
}

func q(kv ...string) string {
	v := url.Values{}
	for i := 0; i < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return "?" + v.Encode()
}

type errBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Offset *int64 `json:"offset"`
}

// wantErr checks the status and the error code of a JSON error response.
func wantErr(t *testing.T, w *httptest.ResponseRecorder, status int, code string) errBody {
	t.Helper()
	var b errBody
	if w.Code != status {
		t.Fatalf("status = %d, want %d %s; body %q", w.Code, status, code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("error Content-Type = %q", ct)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("error body %q: %v", w.Body.String(), err)
	}
	if b.Error.Code != code {
		t.Fatalf("code = %q, want %q", b.Error.Code, code)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff missing")
	}
	return b
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body %q", w.Code, status, w.Body.String())
	}
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return v
}

func lookupMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoadConfigDefaults(t *testing.T) {
	c, err := loadConfig(lookupMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if c.addr != "0.0.0.0:8080" || c.appDir != "/opt/rustgis/app" || c.dataStore != "/data-store" ||
		c.zone != "iplant" || c.user != "" || c.scratch != filepath.Join(wd, "scratch") || c.readOnly ||
		c.publicHost != "" || c.maxChunk != 8388608 || c.maxUpload != 0 || c.maxProject != 268435456 ||
		c.version != "dev" || c.upstream != "unknown" || c.mode != "launch" {
		t.Errorf("defaults: %+v", c)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	for _, m := range []map[string]string{
		{"RGIS_MAX_CHUNK": "abc"},
		{"RGIS_MAX_CHUNK": "262143"},
		{"RGIS_MAX_UPLOAD": "-1"},
		{"RGIS_MAX_PROJECT": "1e6"},
		{"RGIS_MODE": "other"},
	} {
		if _, err := loadConfig(lookupMap(m)); err == nil {
			t.Errorf("%v: no error", m)
		}
	}
	if c, err := loadConfig(lookupMap(map[string]string{"RGIS_MAX_CHUNK": "262144", "RGIS_MODE": "bridge"})); err != nil || c.maxChunk != 262144 || c.mode != "bridge" {
		t.Errorf("minimum chunk: %+v, %v", c, err)
	}
}

func TestLoadConfigUser(t *testing.T) {
	for _, tc := range []struct{ rgis, iplant, want string }{
		{"", "alice", "alice"},
		{"carol", "alice", "carol"},
		{"a.b_c@d-e", "", "a.b_c@d-e"},
		{"a b", "", ""},
		{"a\\b", "", ""},
		{".hidden", "", ""},
		{"shared", "", ""},
		{"a/b", "", ""},
		{"..", "", ""},
		{strings.Repeat("a", 65), "", ""},
		{strings.Repeat("a", 64), "", strings.Repeat("a", 64)},
	} {
		c, err := loadConfig(lookupMap(map[string]string{"RGIS_USER": tc.rgis, "IPLANT_USER": tc.iplant}))
		if err != nil || c.user != tc.want {
			t.Errorf("RGIS_USER=%q IPLANT_USER=%q: user %q, want %q (%v)", tc.rgis, tc.iplant, c.user, tc.want, err)
		}
	}
}

func TestLoadConfigPublicURLAndFlags(t *testing.T) {
	for _, tc := range []struct {
		vars         map[string]string
		scheme, host string
	}{
		{map[string]string{"REDIRECT_URL": "https://a1b2c3d4e.cyverse.run/x?y"}, "https", "a1b2c3d4e.cyverse.run"},
		{map[string]string{"REDIRECT_URL": "https://a.example", "RGIS_PUBLIC_URL": "http://b.example:81"}, "http", "b.example:81"},
		{map[string]string{"REDIRECT_URL": "https://a.example", "RGIS_PUBLIC_URL": ""}, "", ""},
		{map[string]string{"RGIS_PUBLIC_URL": "a.example"}, "", ""},
		{map[string]string{"RGIS_PUBLIC_URL": "ftp://a.example"}, "", ""},
		{map[string]string{"RGIS_PUBLIC_URL": "https:///path"}, "", ""},
	} {
		c, err := loadConfig(lookupMap(tc.vars))
		if err != nil || c.publicScheme != tc.scheme || c.publicHost != tc.host {
			t.Errorf("%v: %q %q (%v)", tc.vars, c.publicScheme, c.publicHost, err)
		}
	}
	for v, want := range map[string]bool{"1": true, "true": true, "TRUE": true, "True": true, "0": false, "yes": false, "": false} {
		c, _ := loadConfig(lookupMap(map[string]string{"RGIS_READONLY": v}))
		if c.readOnly != want {
			t.Errorf("RGIS_READONLY=%q: %v", v, c.readOnly)
		}
	}
	c, _ := loadConfig(lookupMap(map[string]string{"RGIS_ALLOWED_HOSTS": " a.example , b.example:8443,,"}))
	if len(c.allowedHosts) != 2 || c.allowedHosts[0] != "a.example" || c.allowedHosts[1] != "b.example:8443" {
		t.Errorf("allowed hosts: %q", c.allowedHosts)
	}
}

func TestValidPath(t *testing.T) {
	long := strings.Repeat("a", 255)
	valid := []string{"/a", "/a/b c/d.tif", "/a/é.tif", "/" + long, "/a/b.irodssymlink.txt", "/a..b/c"}
	invalid := []string{
		"", "/", "a", "a/b", "/a/", "//a", "/a//b", "/a/./b", "/a/../b", "/..", "/a/..",
		"/a/.hidden", "/.a", "/a\x00b", "/a\\b", "/a\nb", "/a\x7fb", "/a\x1fb", "/a/\xff",
		"/a/x.irodssymlink", "/a/x.IRODSSymlink", "/x.irodssymlink/b",
		"/" + long + "a", "/" + strings.Repeat(long+"/", 17),
	}
	for _, p := range valid {
		if !validPath(p) {
			t.Errorf("validPath(%q) = false", p)
		}
	}
	for _, p := range invalid {
		if validPath(p) {
			t.Errorf("validPath(%q) = true", p)
		}
	}
}

func TestHealthcheckCommand(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.h)
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	if got := healthcheck("0.0.0.0:" + port); got != 0 {
		t.Errorf("healthy server: exit %d", got)
	}
	srv.Close()
	stderr := os.Stderr
	os.Stderr, _ = os.Open(os.DevNull)
	defer func() { os.Stderr = stderr }()
	if got := healthcheck("0.0.0.0:" + port); got != 1 {
		t.Errorf("stopped server: exit %d", got)
	}
	if got := healthcheck("no-port"); got != 1 {
		t.Errorf("bad address: exit %d", got)
	}
}

func TestRequestLogHasNoQueryOrHeaders(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(io.Discard)
	e.get("/api/v1/stat"+q("path", home+"/dem.tif"), "Cookie", "sessionsecret")
	line := buf.String()
	for _, want := range []string{"GET", `"/api/v1/stat"`, " 200 "} {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q lacks %q", line, want)
		}
	}
	for _, leak := range []string{"dem.tif", "sessionsecret", "?"} {
		if strings.Contains(line, leak) {
			t.Errorf("log line %q contains %q", line, leak)
		}
	}
}

// A real connection, so that net/http's own behaviour is covered too.
func TestNoRedirectsOverHTTP(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, p := range []string{
		"/index.html", "/index.html/", "/host", "/host/", "/host/../index.html", "/./index.html",
		"/fs", "/fs/", "/fs" + home, "/fs" + home + "/", "/fs" + home + "/sub", "/fs" + home + "/sub/",
		"/fs/" + home + "/dem.tif", "/fs" + home + "/./dem.tif", "/fs" + home + "/sub/../dem.tif",
		"/api", "/api/", "/api/v1/list/", "/api/v1/session/", "/api//v1/session", "/api/v1/project" + home + "/",
	} {
		for _, method := range []string{"GET", "HEAD"} {
			req, _ := http.NewRequest(method, srv.URL, nil)
			req.URL.Opaque = p // sent byte for byte, without client-side cleaning
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", method, p, err)
			}
			resp.Body.Close()
			if resp.StatusCode/100 == 3 || resp.Header.Get("Location") != "" {
				t.Errorf("%s %s: %d Location %q", method, p, resp.StatusCode, resp.Header.Get("Location"))
			}
			if p != "/index.html" && resp.StatusCode/100 != 4 {
				t.Errorf("%s %s: %d, want 4xx", method, p, resp.StatusCode)
			}
		}
	}
}
