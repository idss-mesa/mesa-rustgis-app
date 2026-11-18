// Copyright The MESA RustGIS App authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

const (
	hashedJS   = "rustgis-web-0123456789abcdef.js"
	hashedWasm = "rustgis-web-0123456789abcdef_bg.wasm"
)

func gz(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(data)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newBundle is an env whose app directory looks like the image's: hashed
// assets and text files with .gz siblings, a service worker, the overlay.
func newBundle(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	files := map[string]string{
		"index.html":               "<!doctype html><title>RustGIS</title>" + strings.Repeat(" ", 200),
		hashedJS:                   "export default function init() {}" + strings.Repeat("/", 200),
		hashedWasm:                 "\x00asm\x01\x00\x00\x00" + strings.Repeat("\x00", 200),
		"sw.js":                    "self.addEventListener('fetch', () => {});",
		"manifest.webmanifest":     `{"name":"RustGIS"}`,
		"host/rustgis-host.js":     "(function () { 'use strict'; })();",
		"host/rustgis-host.css":    "#rgis-host-root { position: fixed; }",
		"THIRD_PARTY_LICENSES.txt": "licenses",
	}
	for name, content := range files {
		e.write(e.app+"/"+name, []byte(content))
		e.write(e.app+"/"+name+".gz", gz(t, []byte(content)))
	}
	e.write(e.app+"/icon-192.png", []byte("\x89PNG"))
	e.write(e.app+"/favicon.svg", []byte("<svg/>"))
	e.write(e.app+"/data.json", []byte("{}"))
	e.write(e.app+"/LICENSE", []byte("Apache"))
	e.write(e.app+"/blob.bin", []byte("<html>"))
	e.write(e.app+"/.secret", []byte("hidden"))
	return e
}

func TestStaticIndex(t *testing.T) {
	e := newBundle(t)
	index := string(mustRead(t, e.app+"/index.html"))
	for _, p := range []string{"/", "/index.html", "/?data=https%3A%2F%2Fx%2Fa.tif&webgl", "/index.html?project=x", "/?"} {
		w := e.get(p)
		wantStatus(t, w, 200)
		if w.Body.String() != index {
			t.Errorf("%s: body %q", p, w.Body.String())
		}
		for k, want := range map[string]string{
			"Content-Type":            "text/html; charset=utf-8",
			"Cache-Control":           "no-cache",
			"Content-Security-Policy": "frame-ancestors 'self'",
			"X-Content-Type-Options":  "nosniff",
			"Vary":                    "Accept-Encoding",
			"Content-Encoding":        "",
			"Content-Disposition":     "",
		} {
			if got := w.Header().Get(k); got != want {
				t.Errorf("%s: %s = %q, want %q", p, k, got, want)
			}
		}
	}
	w := e.do("HEAD", "/", nil)
	wantStatus(t, w, 200)
	if w.Body.Len() != 0 || w.Header().Get("Content-Length") != fmt.Sprint(len(index)) {
		t.Errorf("HEAD /: body %q, length %q", w.Body.String(), w.Header().Get("Content-Length"))
	}
}

func TestStaticCacheHeaders(t *testing.T) {
	e := newBundle(t)
	const immutable = "public, max-age=31536000, immutable"
	e.write(e.app+"/app-0123abc.js", []byte("short hash"))
	e.write(e.app+"/app-0123456G.js", []byte("not hex"))
	e.write(e.app+"/style-0123456789abcdef.css", []byte("css"))
	e.write(e.app+"/snippets-0123456789abcdef.js.map", []byte("map"))
	for p, want := range map[string]string{
		"/" + hashedJS:                      immutable,
		"/" + hashedWasm:                    immutable,
		"/index.html":                       "no-cache",
		"/":                                 "no-cache",
		"/sw.js":                            "no-cache",
		"/manifest.webmanifest":             "no-cache",
		"/host/rustgis-host.js":             "no-cache",
		"/host/rustgis-host.css":            "no-cache",
		"/icon-192.png":                     "no-cache",
		"/app-0123abc.js":                   "no-cache",
		"/app-0123456G.js":                  "no-cache",
		"/style-0123456789abcdef.css":       "no-cache",
		"/snippets-0123456789abcdef.js.map": "no-cache",
		"/missing-0123456789abcdef.js":      "no-cache",
	} {
		w := e.get(p)
		if got := w.Header().Get("Cache-Control"); got != want {
			t.Errorf("%s: Cache-Control %q, want %q", p, got, want)
		}
		if got := w.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'self'" {
			t.Errorf("%s: CSP %q", p, got)
		}
	}
}

func TestStaticPrecompressed(t *testing.T) {
	e := newBundle(t)
	for _, name := range []string{hashedWasm, hashedJS, "index.html", "host/rustgis-host.css"} {
		plain := mustRead(t, e.app+"/"+name)
		packed := mustRead(t, e.app+"/"+name+".gz")
		ctype := typeFor(staticTypes, name)

		for _, ae := range []string{"gzip", "gzip, deflate, br, zstd", "br;q=1.0, GZIP;q=0.5", "deflate,gzip ; q=0.001", "x, gzip;q=1"} {
			w := e.get("/"+name, "Accept-Encoding", ae)
			wantStatus(t, w, 200)
			h := w.Header()
			if h.Get("Content-Encoding") != "gzip" || h.Get("Vary") != "Accept-Encoding" || h.Get("Content-Type") != ctype ||
				h.Get("Content-Length") != fmt.Sprint(len(packed)) {
				t.Errorf("%s (%s): headers %v", name, ae, h)
			}
			if !bytes.Equal(w.Body.Bytes(), packed) {
				t.Errorf("%s (%s): body is not the .gz sibling", name, ae)
			}
			zr, err := gzip.NewReader(w.Body)
			if err != nil {
				t.Fatal(err)
			}
			// The browser checks subresource integrity on these decoded bytes.
			if decoded, _ := io.ReadAll(zr); !bytes.Equal(decoded, plain) {
				t.Errorf("%s: decoded body differs from the original", name)
			}
		}
		for _, ae := range []string{"", "identity", "br", "gzip;q=0", "gzip; q=0.0", "deflate, gzip;q=0.000", "*", "gzipped"} {
			var hdr []string
			if ae != "" {
				hdr = []string{"Accept-Encoding", ae}
			}
			w := e.get("/"+name, hdr...)
			wantStatus(t, w, 200)
			h := w.Header()
			if h.Get("Content-Encoding") != "" || h.Get("Vary") != "Accept-Encoding" || h.Get("Content-Type") != ctype {
				t.Errorf("%s (%q): headers %v", name, ae, h)
			}
			if !bytes.Equal(w.Body.Bytes(), plain) {
				t.Errorf("%s (%q): body is not the original", name, ae)
			}
		}
	}

	// No sibling: no Vary, never an encoding.
	w := e.get("/icon-192.png", "Accept-Encoding", "gzip")
	if w.Code != 200 || w.Header().Get("Vary") != "" || w.Header().Get("Content-Encoding") != "" {
		t.Errorf("file without a sibling: %d %v", w.Code, w.Header())
	}
	// A .gz without its original is not a file of the bundle.
	e.write(e.app+"/orphan.js.gz", gz(t, []byte("x")))
	w = e.get("/orphan.js", "Accept-Encoding", "gzip")
	if w.Code != 404 || w.Header().Get("Content-Encoding") != "" {
		t.Errorf("orphan sibling: %d %v", w.Code, w.Header())
	}
	// HEAD reports the encoded representation.
	w = e.do("HEAD", "/"+hashedWasm, nil, "Accept-Encoding", "gzip")
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Encoding") != "gzip" ||
		w.Header().Get("Content-Length") != fmt.Sprint(len(mustRead(t, e.app+"/"+hashedWasm+".gz"))) {
		t.Errorf("HEAD: %d %v", w.Code, w.Header())
	}
}

func TestStaticETag(t *testing.T) {
	e := newBundle(t)
	stat := func(name string) string {
		fi, err := os.Stat(e.app + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf(`"%x-%x"`, fi.ModTime().UnixNano(), fi.Size())
	}
	plain := e.get("/" + hashedJS)
	packed := e.get("/"+hashedJS, "Accept-Encoding", "gzip")
	if got := plain.Header().Get("ETag"); got != stat(hashedJS) {
		t.Errorf("plain ETag %s", got)
	}
	if got := packed.Header().Get("ETag"); got != stat(hashedJS+".gz") {
		t.Errorf("gzip ETag %s", got)
	}
	if plain.Header().Get("ETag") == packed.Header().Get("ETag") {
		t.Error("both representations share an ETag")
	}
	w := e.get("/"+hashedJS, "Accept-Encoding", "gzip", "If-None-Match", packed.Header().Get("ETag"))
	if w.Code != 304 || w.Body.Len() != 0 {
		t.Errorf("conditional request: %d", w.Code)
	}
	wantStatus(t, e.get("/"+hashedJS, "If-None-Match", packed.Header().Get("ETag")), 200)

	// A range applies to the plain file; the encoded one is always sent whole.
	w = e.get("/"+hashedJS, "Range", "bytes=0-3")
	if w.Code != 206 || w.Body.Len() != 4 {
		t.Errorf("range on the plain file: %d, %d bytes", w.Code, w.Body.Len())
	}
	w = e.get("/"+hashedJS, "Range", "bytes=0-3", "Accept-Encoding", "gzip")
	if w.Code != 200 || w.Body.Len() != packed.Body.Len() || w.Header().Get("Content-Range") != "" {
		t.Errorf("range on the encoded file: %d, %d bytes", w.Code, w.Body.Len())
	}
}

func TestStaticContentTypes(t *testing.T) {
	e := newBundle(t)
	for p, want := range map[string]string{
		"/index.html":               "text/html; charset=utf-8",
		"/" + hashedJS:              "text/javascript; charset=utf-8",
		"/host/rustgis-host.css":    "text/css; charset=utf-8",
		"/" + hashedWasm:            "application/wasm",
		"/manifest.webmanifest":     "application/manifest+json",
		"/data.json":                "application/json",
		"/THIRD_PARTY_LICENSES.txt": "text/plain; charset=utf-8",
		"/LICENSE":                  "text/plain; charset=utf-8",
		"/icon-192.png":             "image/png",
		"/favicon.svg":              "image/svg+xml",
		"/blob.bin":                 "application/octet-stream",
	} {
		w := e.get(p)
		wantStatus(t, w, 200)
		if got := w.Header().Get("Content-Type"); got != want {
			t.Errorf("%s: Content-Type %q, want %q", p, got, want)
		}
	}
}

func TestStaticNotFound(t *testing.T) {
	e := newBundle(t)
	os.Symlink(e.outside, e.app+"/host/out")
	for _, p := range []string{
		"/missing.js", "/host", "/host/", "/index.html/", "/index.html/x", "//", "//index.html", "/host//rustgis-host.js",
		"/.secret", "/%2esecret", "/host/.x", "/..", "/../index.html", "/host/../index.html", "/%2e%2e/index.html",
		"/./index.html", "/index.html.gz", "/" + hashedWasm + ".gz", "/host/rustgis-host.js.gz", "/missing.gz",
		"/index.html%00", "/host%5crustgis-host.js", "/favicon.ico", "/url-ready", "/fsx", "/apix", "/healthz/",
		"/some/deep/client/route",
	} {
		w := e.get(p, "Accept-Encoding", "gzip")
		wantStatus(t, w, 404)
		h := w.Header()
		if !strings.HasPrefix(h.Get("Content-Type"), "text/plain") || h.Get("Content-Encoding") != "" ||
			h.Get("Vary") != "" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: headers %v", p, h)
		}
		if strings.Contains(w.Body.String(), "RustGIS") || strings.Contains(w.Body.String(), "hidden") {
			t.Errorf("%s: fell back to a file", p)
		}
	}
}

func TestStaticMethods(t *testing.T) {
	e := newBundle(t)
	for _, m := range []string{"POST", "PUT", "DELETE", "OPTIONS", "PATCH"} {
		for _, p := range []string{"/", "/index.html", "/healthz", "/host/rustgis-host.js", "/missing"} {
			w := e.do(m, p, []byte("x"), "X-RGIS-Host", "1")
			if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
				t.Errorf("%s %s: %d Allow %q", m, p, w.Code, w.Header().Get("Allow"))
			}
		}
	}
}

func TestHealthz(t *testing.T) {
	e := newEnv(t)
	w := e.get("/healthz")
	wantStatus(t, w, 200)
	if w.Body.String() != "ok\n" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("body %q, type %q", w.Body.String(), w.Header().Get("Content-Type"))
	}
	w = e.do("HEAD", "/healthz", nil)
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD: %d %q", w.Code, w.Body.String())
	}
	wantStatus(t, e.get("/healthz?x=1"), 200)

	// Healthy without a bundle; the shell itself is then a 404, not a crash.
	bare := newEnv(t, "RGIS_APP_DIR", "/nonexistent/app")
	wantStatus(t, bare.get("/healthz"), 200)
	wantStatus(t, bare.get("/"), 404)
}
