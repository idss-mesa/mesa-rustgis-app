// Copyright The MESA RustGIS App authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRewriteProject(t *testing.T) {
	for _, tc := range []struct{ name, host, cur, in, want string }{
		{"placeholder", "example.org", "https://example.org/fs/",
			`{"u":"https://rgis-host.invalid/fs/iplant/home/alice/a%20b.tif"}`,
			`{"u":"https://example.org/fs/iplant/home/alice/a%20b.tif"}`},
		{"stale analysis host", "a1b2c3d4e.cyverse.run", "https://a1b2c3d4e.cyverse.run/fs/",
			`["https://a0000ffff.cyverse.run/fs/output/x.tif","http://adeadbeef.cyverse.run:8443/fs/y","https://rgis-host.invalid/fs/z"]`,
			`["https://a1b2c3d4e.cyverse.run/fs/output/x.tif","https://a1b2c3d4e.cyverse.run/fs/y","https://a1b2c3d4e.cyverse.run/fs/z"]`},
		{"current origin is kept", "a1b2c3d4e.cyverse.run", "https://a1b2c3d4e.cyverse.run/fs/",
			`https://a1b2c3d4e.cyverse.run/fs/a`, `https://a1b2c3d4e.cyverse.run/fs/a`},
		{"request host with port and case", "A1B2C3D4E.Cyverse.Run:443", "https://A1B2C3D4E.Cyverse.Run:443/fs/",
			`https://a0000ffff.cyverse.run/fs/a`, `https://A1B2C3D4E.Cyverse.Run:443/fs/a`},
		{"other hosts are untouched", "a1b2c3d4e.cyverse.run", "https://a1b2c3d4e.cyverse.run/fs/",
			`https://a0000ffff.cyverse.org/fs/a https://a0000ffff.evil-cyverse.run/fs/a https://a0000ffff.cyverseXrun/fs/a ` +
				`https://b0000ffff.cyverse.run/fs/a https://a0000fff.cyverse.run/fs/a https://a0000ffff0.cyverse.run/fs/a ` +
				`https://a0000FFFF.cyverse.run/fs/a https://data.example/fs/a https://a0000ffff.cyverse.run/tiles/a ` +
				`ftp://a0000ffff.cyverse.run/fs/a https://a0000ffff.cyverse.run.evil.example/fs/a http://localhost:8080/fs/a`,
			`https://a0000ffff.cyverse.org/fs/a https://a0000ffff.evil-cyverse.run/fs/a https://a0000ffff.cyverseXrun/fs/a ` +
				`https://b0000ffff.cyverse.run/fs/a https://a0000fff.cyverse.run/fs/a https://a0000ffff0.cyverse.run/fs/a ` +
				`https://a0000FFFF.cyverse.run/fs/a https://data.example/fs/a https://a0000ffff.cyverse.run/tiles/a ` +
				`ftp://a0000ffff.cyverse.run/fs/a https://a0000ffff.cyverse.run.evil.example/fs/a http://localhost:8080/fs/a`},
		{"only the prefix changes", "a1b2c3d4e.cyverse.run", "https://a1b2c3d4e.cyverse.run/fs/",
			`https://a0000ffff.cyverse.run/fs/iplant/home/alice/fs/a%2520b.tif?x=https://a0000ffff.cyverse.run/other`,
			`https://a1b2c3d4e.cyverse.run/fs/iplant/home/alice/fs/a%2520b.tif?x=https://a0000ffff.cyverse.run/other`},
		{"localhost", "localhost:8081", "http://localhost:8081/fs/",
			`http://localhost:8080/fs/a https://127.0.0.1/fs/b http://127.0.0.1:9/fs/c http://localhost.evil/fs/d https://a0000ffff.cyverse.run/fs/e http://127x0.0.1/fs/f`,
			`http://localhost:8081/fs/a http://localhost:8081/fs/b http://localhost:8081/fs/c http://localhost.evil/fs/d https://a0000ffff.cyverse.run/fs/e http://127x0.0.1/fs/f`},
		{"loopback address", "127.0.0.1:8080", "http://127.0.0.1:8080/fs/",
			`http://localhost:1234/fs/a`, `http://127.0.0.1:8080/fs/a`},
		{"any other host rewrites the placeholder only", "gis.example.org", "https://gis.example.org/fs/",
			`http://localhost:8080/fs/a https://a0000ffff.cyverse.run/fs/b https://rgis-host.invalid/fs/c`,
			`http://localhost:8080/fs/a https://a0000ffff.cyverse.run/fs/b https://gis.example.org/fs/c`},
		{"replacement is literal", "localhost", "http://localhost/fs/",
			`http://127.0.0.1/fs/$1${0}`, `http://localhost/fs/$1${0}`},
		{"not JSON", "localhost", "http://localhost/fs/", "\x00\xff{", "\x00\xff{"},
	} {
		if got := string(rewriteProject([]byte(tc.in), tc.host, tc.cur)); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
}

func TestProjectRoute(t *testing.T) {
	e := newEnv(t, "RGIS_ALLOWED_HOSTS", "a1b2c3d4e.cyverse.run")
	const stale = `{"layers":[{"url":"https://a0000ffff.cyverse.run/fs/iplant/home/alice/dem.tif"},{"url":"https://rgis-host.invalid/fs/output/o.fgb"},{"url":"https://data.example/x.tif"}]}`
	const fresh = `{"layers":[{"url":"https://a1b2c3d4e.cyverse.run/fs/iplant/home/alice/dem.tif"},{"url":"https://a1b2c3d4e.cyverse.run/fs/output/o.fgb"},{"url":"https://data.example/x.tif"}]}`
	e.write(e.ds+home+"/my map.rgis", []byte(stale))
	e.write(e.ds+home+"/UPPER.RGIS", []byte(stale))
	e.write(e.ds+home+"/pkg.rgispkg", []byte(stale))
	target := "/api/v1/project" + home + "/my%20map.rgis"
	hdr := []string{"Host", "a1b2c3d4e.cyverse.run", "X-Forwarded-Proto", "https"}

	w := e.get(target, append(hdr, "Range", "bytes=0-3", "Accept-Encoding", "gzip")...)
	wantStatus(t, w, 200)
	if w.Body.String() != fresh {
		t.Errorf("body %s", w.Body.String())
	}
	for k, want := range map[string]string{
		"Content-Type":                 "application/json",
		"Content-Length":               fmt.Sprint(len(fresh)),
		"Content-Disposition":          "attachment",
		"Content-Security-Policy":      lockedCSPWant,
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cache-Control":                "no-store",
		"X-Content-Type-Options":       "nosniff",
		"Accept-Ranges":                "",
		"ETag":                         "",
		"Content-Range":                "",
		"Content-Encoding":             "",
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	// HEAD does not read the file, so it has no length to give.
	w = e.do("HEAD", target, nil, hdr...)
	wantStatus(t, w, 200)
	if w.Body.Len() != 0 || w.Header().Get("Content-Length") != "" || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Content-Disposition") != "attachment" {
		t.Errorf("HEAD: body %q, headers %v", w.Body.String(), w.Header())
	}
	if w := e.do("HEAD", "/api/v1/project"+home+"/missing.rgis", nil, hdr...); w.Code != 404 {
		t.Errorf("HEAD of a missing project: %d", w.Code)
	}

	// The scheme follows the public URL; the host is the request's.
	pub := newEnv(t, "REDIRECT_URL", "https://a1b2c3d4e.cyverse.run")
	pub.write(pub.ds+home+"/p.rgis", []byte(stale))
	if got := pub.get("/api/v1/project"+home+"/p.rgis", "Host", "a1b2c3d4e.cyverse.run").Body.String(); got != fresh {
		t.Errorf("with REDIRECT_URL: %s", got)
	}

	// Locally, a project saved on another port follows the current one.
	e.write(e.ds+"/output/local.rgis", []byte(`"http://localhost:9000/fs/output/a.tif" "https://rgis-host.invalid/fs/b"`))
	if got := e.get("/api/v1/project/output/local.rgis").Body.String(); got != `"http://localhost:8080/fs/output/a.tif" "http://localhost:8080/fs/b"` {
		t.Errorf("local rewrite: %s", got)
	}

	wantStatus(t, e.get("/api/v1/project"+home+"/UPPER.RGIS", hdr...), 200)
	// The file on disk is never changed.
	if string(mustRead(t, e.ds+home+"/my map.rgis")) != stale {
		t.Error("project file modified")
	}
	// Through /fs the same file is served as it is.
	if got := e.get("/fs" + home + "/my%20map.rgis").Body.String(); got != stale {
		t.Errorf("/fs rewrote the project: %s", got)
	}

	wantErr(t, e.get("/api/v1/project"+home+"/pkg.rgispkg"), 400, "bad_request")
	wantErr(t, e.get("/api/v1/project"+home+"/dem.tif"), 400, "bad_request")
	wantErr(t, e.get("/api/v1/project"+home+"/missing.rgis"), 404, "not_found")
	wantErr(t, e.get("/api/v1/project/nowhere/p.rgis"), 404, "not_found")
	wantErr(t, e.get("/api/v1/project/"), 400, "bad_path")
	wantErr(t, e.get("/api/v1/project"+home+"/.rgis"), 400, "bad_path")
	wantErr(t, e.get("/api/v1/project"+home+"/../alice/my%20map.rgis"), 400, "bad_path")
	wantErr(t, e.get(target, "Sec-Fetch-Site", "cross-site"), 403, "forbidden_origin")
	if err := e.get(target, "Origin", "https://a0000ffff.cyverse.run"); err.Code != 403 || strings.Contains(err.Body.String(), "layers") {
		t.Errorf("sibling analysis read a project: %d", err.Code)
	}
	if got := e.get("/api/v1/project" + home + "/missing.rgis").Header().Get("Content-Disposition"); got != "attachment" {
		t.Error("project error lost its class headers")
	}
	// A Host that is not a plain host name cannot be spliced into the body.
	// G0 refuses such a Host first; an IPv6 loopback literal passes G0.
	wantErr(t, e.get(target, "Host", `evil.example/"x`), 403, "forbidden_origin")
	wantErr(t, e.get(target, "Host", "[::1]:8080"), 400, "bad_request")

	small := newEnv(t, "RGIS_MAX_PROJECT", "20")
	small.write(small.ds+home+"/big.rgis", []byte(strings.Repeat("x", 21)))
	small.write(small.ds+home+"/fits.rgis", []byte(strings.Repeat("x", 20)))
	wantErr(t, small.get("/api/v1/project"+home+"/big.rgis"), 413, "file_too_large")
	wantStatus(t, small.get("/api/v1/project"+home+"/fits.rgis"), 200)
}

// A project with nothing to rewrite is passed on without another copy.
func TestRewriteProjectNoCopy(t *testing.T) {
	for _, in := range []string{`{"layers":[]}`, `{"url":"https://data.example/fs/x.tif"}`} {
		body := []byte(in)
		if out := rewriteProject(body, "localhost:8080", "http://localhost:8080/fs/"); &out[0] != &body[0] {
			t.Errorf("%s: copied", in)
		}
	}
}

// A client that stops reading a project must not hold up the next one.
func TestProjectStalledReaderDoesNotBlock(t *testing.T) {
	e := newEnv(t)
	// Larger than the socket buffers on both sides, so the write blocks.
	big := append([]byte(`{"u":"http://localhost:1/fs/output/a.tif"}`), bytes.Repeat([]byte(" "), 48<<20)...)
	e.write(e.ds+"/output/big.rgis", big)
	e.write(e.ds+"/output/small.rgis", []byte(`{"u":"http://localhost:1/fs/output/a.tif"}`))
	srv := httptest.NewServer(e.h)
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /api/v1/project/output/big.rgis HTTP/1.1\r\nHost: %s\r\n\r\n", srv.Listener.Addr())
	// The status line shows the server has started to write; nothing more is read.
	conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.ReadFull(conn, make([]byte, 12)); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(srv.URL + "/api/v1/project/output/small.rgis")
	if err != nil {
		t.Fatalf("second project request behind a stalled one: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if want := `{"u":"` + srv.URL + `/fs/output/a.tif"}`; resp.StatusCode != 200 || string(body) != want {
		t.Errorf("%d %s, want %s", resp.StatusCode, body, want)
	}
}

func TestProjectWriteTime(t *testing.T) {
	if got := projectWriteTime(0); got != 30*time.Second {
		t.Errorf("empty: %v", got)
	}
	if got := projectWriteTime(256 << 20); got != 30*time.Second+4096*time.Second {
		t.Errorf("256 MiB: %v", got)
	}
}
