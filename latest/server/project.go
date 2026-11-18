// Copyright The MESA RustGIS App authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const placeholder = "https://rgis-host.invalid/fs/"

var (
	analysisHostRE = regexp.MustCompile(`^a[0-9a-f]{8}\.(.+)$`)
	localFSRE      = regexp.MustCompile(`https?://(localhost|127\.0\.0\.1)(:[0-9]{1,5})?/fs/`)
	// The origin is spliced into the project unescaped, so the request Host
	// must not be able to carry anything but a host name and port.
	plainHostRE = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
)

// rewriteProject points stale /fs/ prefixes at cur, the current origin's. It
// returns body itself when there is nothing to rewrite.
func rewriteProject(body []byte, host, cur string) []byte {
	if !bytes.Contains(body, []byte("/fs/")) {
		return body
	}
	if bytes.Contains(body, []byte(placeholder)) {
		body = bytes.ReplaceAll(body, []byte(placeholder), []byte(cur))
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	var stale *regexp.Regexp
	if m := analysisHostRE.FindStringSubmatch(host); m != nil {
		stale = regexp.MustCompile(`https?://a[0-9a-f]{8}\.` + regexp.QuoteMeta(m[1]) + `(:[0-9]{1,5})?/fs/`)
	} else if host == "localhost" || host == "127.0.0.1" {
		stale = localFSRE
	}
	if stale == nil || !stale.Match(body) {
		return body
	}
	return stale.ReplaceAllLiteral(body, []byte(cur))
}

var errProjectTooLarge = &apiError{status: http.StatusRequestEntityTooLarge, code: "file_too_large", msg: "project is too large to rewrite"}

// readProject returns the rewritten project. The file is held in memory, so
// one rewrite runs at a time.
func (s *server) readProject(f *os.File, size int64, host, cur string) ([]byte, *apiError) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	var buf bytes.Buffer
	buf.Grow(int(size) + bytes.MinRead)
	if _, err := buf.ReadFrom(io.LimitReader(f, s.cfg.maxProject+1)); err != nil {
		return nil, fsError(err)
	}
	if int64(buf.Len()) > s.cfg.maxProject {
		return nil, errProjectTooLarge
	}
	return rewriteProject(buf.Bytes(), host, cur), nil
}

// projectWriteTime is how long a client may take over a project of n bytes:
// 30 s plus the time n bytes need at 64 KiB/s.
func projectWriteTime(n int) time.Duration {
	return 30*time.Second + time.Duration(n>>16)*time.Second
}

func (s *server) apiProject(w http.ResponseWriter, r *http.Request) {
	vpath := strings.TrimPrefix(r.URL.Path, "/api/v1/project")
	switch {
	case !validPath(vpath):
		writeError(w, r, errBadPath)
		return
	case !strings.EqualFold(vpath[max(0, len(vpath)-5):], ".rgis"):
		writeError(w, r, badRequest("not a .rgis project"))
		return
	case !plainHostRE.MatchString(r.Host):
		writeError(w, r, badRequest("unusable Host header"))
		return
	}
	f, fi, e := s.openFile(vpath)
	if e != nil {
		writeError(w, r, e)
		return
	}
	defer f.Close()
	if fi.Size() > s.cfg.maxProject {
		writeError(w, r, errProjectTooLarge)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodHead {
		// The length is known only after the rewrite, which a HEAD is not worth.
		return
	}
	body, e := s.readProject(f, fi.Size(), r.Host, s.origin(r)+"/fs/")
	if e != nil {
		writeError(w, r, e)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	// The buffer lives until the client has taken it, so a client that stops
	// reading is cut off. The server has no write timeout of its own, and a
	// deadline left on a kept-alive connection would hit its next response.
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Now().Add(projectWriteTime(len(body))))
	defer rc.SetWriteDeadline(time.Time{})
	w.Write(body)
}
