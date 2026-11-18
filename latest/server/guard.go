// Copyright The MESA RustGIS App authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// guard applies rules G0 to G5. It runs on every /fs and /api request because
// vice-proxy answers credentialed CORS for sibling analyses and forwards no
// identity.
func (s *server) guard(r *http.Request, mutation bool) *apiError {
	// G0 comes first: a page that reached this server under a name of its own
	// (DNS rebinding) is same-origin to the browser, so G1 and G2 pass it.
	if !s.namedHost(r.Host) && !loopbackHost(r.Host) {
		log.Printf("refused Host %q: not the public URL's host, an RGIS_ALLOWED_HOSTS entry or a loopback name", r.Host)
		return errForbiddenHost
	}
	for _, v := range r.Header.Values("Sec-Fetch-Site") {
		if v != "same-origin" && v != "none" {
			return errForbiddenOrigin
		}
	}
	if origins := r.Header.Values("Origin"); len(origins) > 0 {
		if len(origins) != 1 || !s.allowedOrigin(r, origins[0]) {
			return errForbiddenOrigin
		}
	}
	if !mutation {
		return nil
	}
	if v := r.Header.Values("X-RGIS-Host"); len(v) != 1 || v[0] != "1" {
		return &apiError{status: http.StatusForbidden, code: "missing_header", msg: "X-RGIS-Host: 1 is required"}
	}
	want := ""
	switch r.Method {
	case http.MethodPost:
		want = "application/json"
	case http.MethodPut:
		want = "application/octet-stream"
	}
	if want != "" {
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != want {
			return &apiError{status: http.StatusUnsupportedMediaType, code: "bad_content_type", msg: "Content-Type must be " + want}
		}
	}
	if s.cfg.readOnly {
		return errReadOnly
	}
	return nil
}

// namedHost reports whether host is the public URL's or an RGIS_ALLOWED_HOSTS
// entry.
func (s *server) namedHost(host string) bool {
	if s.cfg.publicHost != "" && strings.EqualFold(host, s.cfg.publicHost) {
		return true
	}
	for _, h := range s.cfg.allowedHosts {
		if strings.EqualFold(host, h) {
			return true
		}
	}
	return false
}

// loopbackHost reports whether host is localhost, 127.0.0.1 or [::1], with
// any port. These are not names an attacker can point at this server.
func loopbackHost(host string) bool {
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.HasSuffix(host, "]") {
		if port := host[i+1:]; port == "" || strings.Trim(port, "0123456789") != "" {
			return false
		}
		host = host[:i]
	}
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "[::1]"
}

// allowedOrigin is G2. r.Host has passed G0.
func (s *server) allowedOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != s.scheme(r) || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host) || s.namedHost(u.Host)
}
