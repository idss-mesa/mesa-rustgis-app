// Copyright The MESA RustGIS App authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var immutableRE = regexp.MustCompile(`-[0-9a-f]{8,}(_bg)?\.(js|wasm)$`)

var staticTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".wasm":        "application/wasm",
	".webmanifest": "application/manifest+json",
	".json":        "application/json",
	".txt":         "text/plain; charset=utf-8",
	"":             "text/plain; charset=utf-8",
	".png":         "image/png",
	".svg":         "image/svg+xml",
}

func typeFor(table map[string]string, name string) string {
	if t, ok := table[strings.ToLower(path.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}

func etag(fi fs.FileInfo) string {
	return fmt.Sprintf(`"%x-%x"`, fi.ModTime().UnixNano(), fi.Size())
}

func acceptsGzip(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept-Encoding") {
		for part := range strings.SplitSeq(v, ",") {
			coding, params, _ := strings.Cut(part, ";")
			if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
				continue
			}
			if q, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(params)), "q="); ok {
				if f, err := strconv.ParseFloat(q, 64); err == nil && f == 0 {
					return false
				}
			}
			return true
		}
	}
	return false
}

// serveStatic mirrors upstream's nginx.conf: hashed assets are immutable,
// everything else revalidates, and .gz siblings are served when accepted.
func (s *server) serveStatic(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "frame-ancestors 'self'")
	h.Set("Cache-Control", "no-cache")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	if p == "/healthz" {
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Content-Length", "3")
		if r.Method == http.MethodGet {
			w.Write([]byte("ok\n"))
		}
		return
	}
	if p == "/" {
		p = "/index.html"
	}
	if !validPath(p) || strings.HasSuffix(p, ".gz") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	name := filepath.Join(s.cfg.appDir, filepath.FromSlash(p))
	if gz, err := os.Stat(name + ".gz"); err == nil && gz.Mode().IsRegular() {
		h.Add("Vary", "Accept-Encoding")
		if fi, err := os.Stat(name); err == nil && fi.Mode().IsRegular() && acceptsGzip(r) {
			name += ".gz"
			h.Set("Content-Encoding", "gzip")
		}
	}
	f, err := os.Open(name)
	if err != nil {
		notFoundStatic(w)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		notFoundStatic(w)
		return
	}
	if immutableRE.MatchString(p) {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	h.Set("Content-Type", typeFor(staticTypes, p))
	h.Set("ETag", etag(fi))
	if h.Get("Content-Encoding") != "" {
		// ServeContent leaves the length out of an encoded response; without it
		// the wasm download is chunked and shows no progress. A range of the
		// encoded bytes is of no use to anyone, so the whole file is sent.
		r.Header.Del("Range")
		h.Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	}
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

func notFoundStatic(w http.ResponseWriter) {
	w.Header().Del("Vary")
	w.Header().Del("Content-Encoding")
	http.Error(w, "not found", http.StatusNotFound)
}
