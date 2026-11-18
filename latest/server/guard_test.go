// Copyright The MESA RustGIS App authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const pubHost = "a1b2c3d4e.cyverse.run"

func TestGuardMatrix(t *testing.T) {
	e := newEnv(t, "REDIRECT_URL", "https://"+pubHost+"/", "RGIS_ALLOWED_HOSTS", "alt.example:8443, other.example")
	e.write(e.ds+home+"/p.rgis", []byte("{}"))
	upload := "/api/v1/upload" + q("path", home+"/g.bin", "id", testID, "offset", "0")

	type route struct {
		name, method, target string
		body                 []byte
		base                 []string
		ok                   int
	}
	mut := []string{"X-RGIS-Host", "1"}
	routes := []route{
		{"fs", "GET", "/fs" + home + "/dem.tif", nil, nil, 200},
		{"fs head", "HEAD", "/fs" + home + "/dem.tif", nil, nil, 200},
		{"session", "GET", "/api/v1/session", nil, nil, 200},
		{"list", "GET", "/api/v1/list" + q("path", home), nil, nil, 200},
		{"stat", "GET", "/api/v1/stat" + q("path", home), nil, nil, 200},
		{"project", "GET", "/api/v1/project" + home + "/p.rgis", nil, nil, 200},
		{"upload head", "HEAD", upload, nil, nil, 200},
		{"unknown api", "GET", "/api/v1/nothing", nil, nil, 404},
		{"fs options", "OPTIONS", "/fs" + home + "/dem.tif", nil, nil, 405},
		{"mkdir", "POST", "/api/v1/mkdir", []byte(`{"path":"/iplant/home/alice/sub"}`), append([]string{"Content-Type", "application/json"}, mut...), 409},
		{"upload put", "PUT", upload, []byte{}, append([]string{"Content-Type", "application/octet-stream"}, mut...), 200},
		{"upload delete", "DELETE", upload, nil, mut, 204},
	}
	type variant struct {
		name string
		hdr  []string
		pass bool
	}
	variants := []variant{
		{"no browser headers", nil, true},
		{"sfs same-origin", []string{"Sec-Fetch-Site", "same-origin"}, true},
		{"sfs none", []string{"Sec-Fetch-Site", "none"}, true},
		{"sfs same-site", []string{"Sec-Fetch-Site", "same-site"}, false},
		{"sfs cross-site", []string{"Sec-Fetch-Site", "cross-site"}, false},
		{"sfs empty", []string{"Sec-Fetch-Site", ""}, false},
		{"sfs upper case", []string{"Sec-Fetch-Site", "Same-Origin"}, false},
		{"sfs unknown", []string{"Sec-Fetch-Site", "same-origin, cross-site"}, false},
		{"sfs given twice", []string{"Sec-Fetch-Site", "same-origin", "Sec-Fetch-Site", "cross-site"}, false},
		{"origin same host", []string{"Origin", "https://" + pubHost}, true},
		{"origin same host, case", []string{"Origin", "https://A1B2C3D4E.CYVERSE.RUN"}, true},
		// The public scheme is https: the http page on the same name is another origin.
		{"origin same host, http", []string{"Origin", "http://" + pubHost}, false},
		{"origin allowed host", []string{"Origin", "https://alt.example:8443"}, true},
		{"origin allowed host 2", []string{"Origin", "https://Other.Example"}, true},
		{"origin allowed host, http", []string{"Origin", "http://alt.example:8443"}, false},
		{"origin sibling analysis", []string{"Origin", "https://a0000ffff.cyverse.run"}, false},
		{"origin other port", []string{"Origin", "https://" + pubHost + ":8443"}, false},
		{"origin loopback", []string{"Origin", "https://localhost:8080"}, false},
		{"origin allowed host, other port", []string{"Origin", "https://alt.example"}, false},
		{"origin suffix trick", []string{"Origin", "https://" + pubHost + ".evil.example"}, false},
		{"origin userinfo trick", []string{"Origin", "https://" + pubHost + "@evil.example"}, false},
		{"origin null", []string{"Origin", "null"}, false},
		{"origin empty", []string{"Origin", ""}, false},
		{"origin bare host", []string{"Origin", pubHost}, false},
		{"origin garbage", []string{"Origin", "https://[::1"}, false},
		{"origin other scheme", []string{"Origin", "file://" + pubHost}, false},
		{"origin given twice", []string{"Origin", "https://" + pubHost, "Origin", "https://evil.example"}, false},
		{"same origin and sfs", []string{"Origin", "https://" + pubHost, "Sec-Fetch-Site", "same-origin"}, true},
		{"same origin, sfs cross-site", []string{"Origin", "https://" + pubHost, "Sec-Fetch-Site", "cross-site"}, false},
		{"foreign origin, sfs same-origin", []string{"Origin", "https://evil.example", "Sec-Fetch-Site", "same-origin"}, false},
	}
	for _, rt := range routes {
		for _, v := range variants {
			w := e.do(rt.method, rt.target, rt.body, append(append([]string{"Host", pubHost}, rt.base...), v.hdr...)...)
			want := rt.ok
			if !v.pass {
				want = 403
			}
			if w.Code != want {
				t.Errorf("%s / %s: status %d, want %d", rt.name, v.name, w.Code, want)
				continue
			}
			if !v.pass && rt.method != "HEAD" {
				wantErr(t, w, 403, "forbidden_origin")
			}
			if !v.pass && strings.Contains(w.Body.String(), string(demBytes)) {
				t.Errorf("%s / %s: refused response carries file content", rt.name, v.name)
			}
		}
	}
	if _, err := os.Stat(e.ds + home + "/g.bin"); err == nil {
		t.Error("an upload was committed")
	}
}

// G0: a page that reached the server under its own name (DNS rebinding) is
// same-origin to the browser, so only the Host gives it away.
func TestGuardHost(t *testing.T) {
	mut := []string{"X-RGIS-Host", "1", "Content-Type", "application/json"}
	check := func(e *env, host string, pass bool) {
		t.Helper()
		// What a browser sends from a page on that host.
		page := []string{"Host", host, "Origin", "http://" + host, "Sec-Fetch-Site", "same-origin"}
		for _, hdr := range [][]string{{"Host", host}, page} {
			fs := e.get("/fs"+home+"/dem.tif", hdr...)
			session := e.get("/api/v1/session", hdr...)
			mkdir := e.do("POST", "/api/v1/mkdir", []byte(`{"path":"/iplant/home/alice/sub"}`), append(mut, hdr...)...)
			if pass {
				wantStatus(t, fs, 200)
				wantStatus(t, session, 200)
				wantErr(t, mkdir, 409, "exists")
				continue
			}
			for _, w := range []*httptest.ResponseRecorder{fs, session, mkdir} {
				wantErr(t, w, 403, "forbidden_origin")
			}
			if strings.Contains(fs.Body.String(), string(demBytes)) || strings.Contains(session.Body.String(), "alice") {
				t.Errorf("Host %q: refused response carries content", host)
			}
		}
		wantStatus(t, e.get("/", "Host", host), 200)
		wantStatus(t, e.get("/healthz", "Host", host), 200)
	}
	loopback := []string{"localhost", "localhost:8080", "LOCALHOST:1", "127.0.0.1", "127.0.0.1:18090", "[::1]", "[::1]:8080"}
	foreign := []string{
		"evil.example", "evil.example:8080", "a0000ffff.cyverse.run", "192.168.1.5:8080", "0.0.0.0:8080", "127.0.0.2:8080",
		"localhost.evil.example", "127.0.0.1.evil.example:8080", "evil.localhost", "localhost:8080.evil.example", "",
	}

	// http here, so that the page's Origin fails on the host alone.
	e := newEnv(t, "REDIRECT_URL", "http://"+pubHost, "RGIS_ALLOWED_HOSTS", "alt.example:8443")
	for _, host := range append([]string{pubHost, "A1B2C3D4E.cyverse.run", "alt.example:8443"}, loopback...) {
		check(e, host, true)
	}
	for _, host := range append([]string{pubHost + ":8080", "alt.example", pubHost + "."}, foreign...) {
		check(e, host, false)
	}

	// Without a public URL only loopback names are served.
	e = newEnv(t, "RGIS_PUBLIC_URL", "")
	for _, host := range loopback {
		check(e, host, true)
	}
	for _, host := range append([]string{pubHost}, foreign...) {
		check(e, host, false)
	}
}

func TestGuardMutationRules(t *testing.T) {
	e := newEnv(t)
	upload := "/api/v1/upload" + q("path", home+"/g.bin", "id", testID, "offset", "0", "commit", "1", "size", "1")
	mkdirBody := []byte(`{"path":"/iplant/home/alice/guarded"}`)
	for _, tc := range []struct {
		name, method, target string
		body                 []byte
		hdr                  []string
		status               int
		code                 string
	}{
		{"mkdir without header", "POST", "/api/v1/mkdir", mkdirBody, []string{"Content-Type", "application/json"}, 403, "missing_header"},
		{"mkdir header true", "POST", "/api/v1/mkdir", mkdirBody, []string{"Content-Type", "application/json", "X-RGIS-Host", "true"}, 403, "missing_header"},
		{"mkdir header 1 twice", "POST", "/api/v1/mkdir", mkdirBody, []string{"Content-Type", "application/json", "X-RGIS-Host", "1", "X-RGIS-Host", "1"}, 403, "missing_header"},
		{"mkdir header padded", "POST", "/api/v1/mkdir", mkdirBody, []string{"Content-Type", "application/json", "X-RGIS-Host", "1, 1"}, 403, "missing_header"},
		{"put without header", "PUT", upload, []byte("x"), []string{"Content-Type", "application/octet-stream"}, 403, "missing_header"},
		{"delete without header", "DELETE", upload, nil, nil, 403, "missing_header"},
		{"mkdir as form", "POST", "/api/v1/mkdir", []byte("path=/output/x"), []string{"X-RGIS-Host", "1", "Content-Type", "application/x-www-form-urlencoded"}, 415, "bad_content_type"},
		{"mkdir as multipart", "POST", "/api/v1/mkdir", mkdirBody, []string{"X-RGIS-Host", "1", "Content-Type", "multipart/form-data; boundary=x"}, 415, "bad_content_type"},
		{"mkdir as text", "POST", "/api/v1/mkdir", mkdirBody, []string{"X-RGIS-Host", "1", "Content-Type", "text/plain"}, 415, "bad_content_type"},
		{"mkdir without type", "POST", "/api/v1/mkdir", mkdirBody, []string{"X-RGIS-Host", "1"}, 415, "bad_content_type"},
		{"mkdir as octet-stream", "POST", "/api/v1/mkdir", mkdirBody, []string{"X-RGIS-Host", "1", "Content-Type", "application/octet-stream"}, 415, "bad_content_type"},
		{"put as text", "PUT", upload, []byte("x"), []string{"X-RGIS-Host", "1", "Content-Type", "text/plain"}, 415, "bad_content_type"},
		{"put as form", "PUT", upload, []byte("x"), []string{"X-RGIS-Host", "1", "Content-Type", "application/x-www-form-urlencoded"}, 415, "bad_content_type"},
		{"put as json", "PUT", upload, []byte("x"), []string{"X-RGIS-Host", "1", "Content-Type", "application/json"}, 415, "bad_content_type"},
		{"put without type", "PUT", upload, []byte("x"), []string{"X-RGIS-Host", "1"}, 415, "bad_content_type"},
		// Order: G1/G2 before G3 before G4.
		{"cross-site, no header", "POST", "/api/v1/mkdir", mkdirBody, []string{"Sec-Fetch-Site", "cross-site", "Content-Type", "text/plain"}, 403, "forbidden_origin"},
		{"no header, form", "POST", "/api/v1/mkdir", mkdirBody, []string{"Content-Type", "application/x-www-form-urlencoded"}, 403, "missing_header"},
	} {
		wantErr(t, e.do(tc.method, tc.target, tc.body, tc.hdr...), tc.status, tc.code)
	}
	for _, name := range []string{"guarded", "g.bin", ".rgis-upload-" + testID + ".part"} {
		if _, err := os.Lstat(e.ds + home + "/" + name); err == nil {
			t.Errorf("%s was created by a refused request", name)
		}
	}
	// Parameters and case of the media type are ignored.
	wantStatus(t, e.do("POST", "/api/v1/mkdir", mkdirBody, "X-RGIS-Host", "1", "Content-Type", "Application/JSON; charset=UTF-8"), 201)
	wantStatus(t, e.do("PUT", upload, []byte("x"), "X-RGIS-Host", "1", "Content-Type", "application/octet-stream; x=y"), 201)
	// DELETE has no body and needs no content type.
	wantStatus(t, e.do("DELETE", upload, nil, "X-RGIS-Host", "1"), 204)

	// G4 before G5; G5 before the path is looked at.
	ro := newEnv(t, "RGIS_READONLY", "1")
	wantErr(t, ro.do("POST", "/api/v1/mkdir", mkdirBody, "X-RGIS-Host", "1", "Content-Type", "text/plain"), 415, "bad_content_type")
	wantErr(t, ro.do("POST", "/api/v1/mkdir", mkdirBody), 403, "missing_header")
	wantErr(t, ro.mkdir("/../x"), 403, "read_only")
}

func TestStaticIsNotGuarded(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/", "/index.html", "/healthz"} {
		w := e.get(p, "Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example")
		wantStatus(t, w, 200)
	}
}

func TestNoCORSHeadersAnywhere(t *testing.T) {
	e := newEnv(t)
	var all []*httptest.ResponseRecorder
	for _, origin := range []string{"http://" + testHost, "https://evil.example"} {
		for _, m := range []string{"GET", "HEAD", "OPTIONS", "POST", "PUT", "DELETE"} {
			for _, p := range []string{"/", "/healthz", "/fs" + home + "/dem.tif", "/api/v1/session", "/api/v1/upload", "/api/v1/mkdir", "/api/v1/x"} {
				all = append(all, e.do(m, p, nil, "Origin", origin, "Access-Control-Request-Method", "PUT"))
			}
		}
	}
	for _, w := range all {
		if w.Code/100 == 3 && w.Code != 304 {
			t.Errorf("redirect %d", w.Code)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Error("nosniff missing")
		}
		for k := range w.Header() {
			if strings.HasPrefix(k, "Access-Control-") {
				t.Errorf("response carries %s", k)
			}
		}
	}
}
