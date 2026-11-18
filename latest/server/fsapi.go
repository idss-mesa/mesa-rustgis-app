// Copyright The MESA RustGIS App authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

type apiError struct {
	status    int
	code, msg string
	offset    *int64
}

func (e *apiError) withOffset(n int64) *apiError {
	c := *e
	c.offset = &n
	return &c
}

var (
	errForbiddenOrigin = &apiError{status: http.StatusForbidden, code: "forbidden_origin", msg: "cross-origin request refused"}
	errForbiddenHost   = &apiError{status: http.StatusForbidden, code: "forbidden_origin", msg: "request Host is not this app's address (RGIS_PUBLIC_URL, RGIS_ALLOWED_HOSTS)"}
	errReadOnly        = &apiError{status: http.StatusForbidden, code: "read_only", msg: "this location is read-only"}
	errForbidden       = &apiError{status: http.StatusForbidden, code: "forbidden", msg: "permission denied"}
	errNotFound        = &apiError{status: http.StatusNotFound, code: "not_found", msg: "no such file or directory"}
	errBadPath         = &apiError{status: http.StatusBadRequest, code: "bad_path", msg: "invalid path"}
	errExists          = &apiError{status: http.StatusConflict, code: "exists", msg: "already exists"}
	errStore           = &apiError{status: http.StatusBadGateway, code: "store_error", msg: "the Data Store refused or failed the operation (permission, quota or connection)"}
)

func badRequest(msg string) *apiError {
	return &apiError{status: http.StatusBadRequest, code: "bad_request", msg: msg}
}

// fsError maps a filesystem error to the contract's codes. Messages are fixed
// so on-disk paths never reach the client; the cause is logged instead.
func fsError(err error) *apiError {
	var errno syscall.Errno
	isErrno := errors.As(err, &errno)
	switch {
	case errors.Is(err, fs.ErrNotExist), errno == syscall.ENOTDIR, errno == syscall.ELOOP, errno == syscall.ENAMETOOLONG:
		return errNotFound
	case !isErrno:
		// os.Root reports a symlink that leaves the root with an unexported,
		// non-errno error; every real I/O failure carries an errno.
		return errNotFound
	case errno == syscall.EACCES, errno == syscall.EPERM, errno == syscall.EROFS:
		return errForbidden
	case errno == syscall.ENOSPC, errno == syscall.EDQUOT:
		return &apiError{status: http.StatusInsufficientStorage, code: "no_space", msg: "no space left"}
	case errno == syscall.EREMOTEIO:
		// irodsfs reports every failure on the iRODS side with this one errno,
		// refused permission and exceeded quota included.
		log.Printf("store error: %v", err)
		return errStore
	}
	log.Printf("io error: %v", err)
	return &apiError{status: http.StatusInternalServerError, code: "io_error", msg: "filesystem error"}
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		log.Printf("json: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

func writeError(w http.ResponseWriter, r *http.Request, e *apiError) {
	if r.Method == http.MethodHead {
		w.WriteHeader(e.status)
		return
	}
	type detail struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	writeJSON(w, r, e.status, struct {
		Error  detail `json:"error"`
		Offset *int64 `json:"offset,omitempty"`
	}{detail{e.code, e.msg}, e.offset})
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, r, &apiError{status: http.StatusMethodNotAllowed, code: "method_not_allowed", msg: "allowed: " + allow})
}

// validName is path rules 2 to 4 for one segment.
func validName(s string) bool {
	if s == "" || len(s) > 255 || s[0] == '.' || !utf8.ValidString(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f || c == '\\' || c == '/' {
			return false
		}
	}
	const hidden = ".irodssymlink"
	return len(s) < len(hidden) || !strings.EqualFold(s[len(s)-len(hidden):], hidden)
}

func validPath(p string) bool {
	if len(p) < 2 || len(p) > 4096 || p[0] != '/' {
		return false
	}
	for seg := range strings.SplitSeq(p[1:], "/") {
		if !validName(seg) {
			return false
		}
	}
	return true
}

func (s *server) writable(rt *root) bool { return rt.writable && !s.cfg.readOnly }

// locate finds the root that holds a valid virtual path and the name relative
// to it, "." for the root itself.
func (s *server) locate(vpath string) (*root, string, bool) {
	for i := range s.roots {
		rt := &s.roots[i]
		if vpath == rt.vpath {
			return rt, ".", true
		}
		if rel, ok := strings.CutPrefix(vpath, rt.vpath+"/"); ok {
			return rt, rel, true
		}
	}
	return nil, "", false
}

// lookup validates and locates vpath, opens its root and applies plainDirs.
// The caller closes the os.Root.
func (s *server) lookup(vpath string) (*root, *os.Root, string, *apiError) {
	if !validPath(vpath) {
		return nil, nil, "", errBadPath
	}
	rt, rel, ok := s.locate(vpath)
	if !ok {
		return nil, nil, "", errNotFound
	}
	dir, err := os.OpenRoot(rt.dir)
	if err != nil {
		return nil, nil, "", fsError(err)
	}
	if e := plainDirs(dir, rel); e != nil {
		dir.Close()
		return nil, nil, "", e
	}
	return rt, dir, rel, nil
}

// plainDirs answers 404 when a directory on the way to rel is a symlink. The
// path rules see only the names in the request, so a link that was followed
// could lead to a name they forbid. The last component is the caller's to
// Lstat; a missing directory is left to the operation that follows.
func plainDirs(dir *os.Root, rel string) *apiError {
	for i := range len(rel) {
		if rel[i] != '/' {
			continue
		}
		fi, err := dir.Lstat(rel[:i])
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil:
			return fsError(err)
		case fi.Mode()&fs.ModeSymlink != 0:
			return errNotFound
		}
	}
	return nil
}

func rootExists(rt *root) bool {
	fi, err := os.Stat(rt.dir)
	return err == nil && fi.IsDir()
}

type entry struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Size  int64  `json:"size"`
	MTime string `json:"mtime"`
}

// newEntry reports false for anything that is neither a regular file nor a
// directory.
func newEntry(name string, fi fs.FileInfo) (entry, bool) {
	e := entry{Name: name, MTime: fi.ModTime().UTC().Format(time.RFC3339)}
	switch {
	case fi.IsDir():
		e.Type = "dir"
	case fi.Mode().IsRegular():
		e.Type, e.Size = "file", fi.Size()
	default:
		return e, false
	}
	return e, true
}

// openNode opens a file or directory read-only. O_NONBLOCK keeps a FIFO from
// blocking the open before its type can be checked.
func openNode(dir *os.Root, rel string) (*os.File, fs.FileInfo, *apiError) {
	// os.Root follows a link that stays inside it, O_NOFOLLOW or not.
	if li, err := dir.Lstat(rel); err != nil {
		return nil, nil, fsError(err)
	} else if li.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, errNotFound
	}
	f, err := dir.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, fsError(err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, fsError(err)
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, errNotFound
	}
	return f, fi, nil
}

// openFile opens the regular file at vpath.
func (s *server) openFile(vpath string) (*os.File, fs.FileInfo, *apiError) {
	_, dir, rel, e := s.lookup(vpath)
	if e != nil {
		return nil, nil, e
	}
	defer dir.Close()
	f, fi, e := openNode(dir, rel)
	if e != nil {
		return nil, nil, e
	}
	if fi.IsDir() {
		f.Close()
		return nil, nil, errNotFound
	}
	return f, fi, nil
}

var fsTypes = map[string]string{
	".tif":     "image/tiff",
	".tiff":    "image/tiff",
	".json":    "application/json",
	".geojson": "application/json",
	".rgis":    "application/json",
	".csv":     "text/csv; charset=utf-8",
	".txt":     "text/plain; charset=utf-8",
	".png":     "image/png",
	".jpg":     "image/jpeg",
	".jpeg":    "image/jpeg",
}

const lockedCSP = "sandbox; default-src 'none'; frame-ancestors 'none'"

func (s *server) serveFS(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Disposition", "attachment")
	h.Set("Content-Security-Policy", lockedCSP)
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cache-Control", "private, no-cache")
	if e := s.guard(r, false); e != nil {
		writeError(w, r, e)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, r, "GET, HEAD")
		return
	}
	vpath := strings.TrimPrefix(r.URL.Path, "/fs")
	if vpath == "" || vpath == "/" {
		writeError(w, r, errNotFound)
		return
	}
	f, fi, e := s.openFile(vpath)
	if e != nil {
		writeError(w, r, e)
		return
	}
	defer f.Close()
	// Set before ServeContent so it never sniffs a type from the content.
	h.Set("Content-Type", typeFor(fsTypes, vpath))
	h.Set("ETag", etag(fi))
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

func (s *server) serveAPI(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", lockedCSP)
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")

	p := r.URL.Path
	project := strings.HasPrefix(p, "/api/v1/project/")
	if project {
		h.Set("Content-Disposition", "attachment")
	}
	mutation := (p == "/api/v1/mkdir" && r.Method == http.MethodPost) ||
		(p == "/api/v1/upload" && (r.Method == http.MethodPut || r.Method == http.MethodDelete))
	if e := s.guard(r, mutation); e != nil {
		if r.Method == http.MethodPut {
			drain(r)
		}
		writeError(w, r, e)
		return
	}

	type route struct {
		allow string
		serve map[string]http.HandlerFunc
	}
	var rt route
	switch {
	case p == "/api/v1/session":
		rt = route{"GET", map[string]http.HandlerFunc{"GET": s.apiSession}}
	case p == "/api/v1/list":
		rt = route{"GET", map[string]http.HandlerFunc{"GET": s.apiList}}
	case p == "/api/v1/stat":
		rt = route{"GET", map[string]http.HandlerFunc{"GET": s.apiStat}}
	case p == "/api/v1/mkdir":
		rt = route{"POST", map[string]http.HandlerFunc{"POST": s.apiMkdir}}
	case p == "/api/v1/upload":
		rt = route{"PUT, HEAD, DELETE", map[string]http.HandlerFunc{"PUT": s.uploadPut, "HEAD": s.uploadHead, "DELETE": s.uploadDelete}}
	case project:
		rt = route{"GET, HEAD", map[string]http.HandlerFunc{"GET": s.apiProject, "HEAD": s.apiProject}}
	case r.Method == http.MethodOptions:
		methodNotAllowed(w, r, "GET, HEAD")
		return
	default:
		writeError(w, r, &apiError{status: http.StatusNotFound, code: "not_found", msg: "no such route"})
		return
	}
	if serve, ok := rt.serve[r.Method]; ok {
		serve(w, r)
		return
	}
	methodNotAllowed(w, r, rt.allow)
}

// query parses the query string strictly; a parameter given twice is refused.
func query(r *http.Request) (url.Values, *apiError) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, badRequest("malformed query string")
	}
	for k, v := range q {
		if len(v) > 1 {
			return nil, badRequest("parameter " + k + " given more than once")
		}
	}
	return q, nil
}

func pathParam(q url.Values) (string, *apiError) {
	if !q.Has("path") {
		return "", badRequest("path is required")
	}
	p := q.Get("path")
	if !validPath(p) {
		return "", errBadPath
	}
	return p, nil
}

func (s *server) apiSession(w http.ResponseWriter, r *http.Request) {
	type rootInfo struct {
		ID       string `json:"id"`
		Label    string `json:"label"`
		Path     string `json:"path"`
		Writable bool   `json:"writable"`
		Exists   bool   `json:"exists"`
	}
	type limits struct {
		MaxChunk   int64 `json:"maxChunk"`
		MaxUpload  int64 `json:"maxUpload"`
		MaxProject int64 `json:"maxProject"`
		MaxOpen    int   `json:"maxOpen"`
	}
	type versions struct {
		API      int    `json:"api"`
		Host     string `json:"host"`
		Upstream string `json:"upstream"`
	}
	roots := make([]rootInfo, len(s.roots))
	for i := range s.roots {
		rt := &s.roots[i]
		roots[i] = rootInfo{rt.id, rt.label, rt.vpath, s.writable(rt), rootExists(rt)}
	}
	writeJSON(w, r, http.StatusOK, struct {
		User     string     `json:"user"`
		Origin   string     `json:"origin"`
		ReadOnly bool       `json:"readOnly"`
		Mode     string     `json:"mode"`
		Roots    []rootInfo `json:"roots"`
		Limits   limits     `json:"limits"`
		Versions versions   `json:"versions"`
	}{
		s.cfg.user, s.origin(r), s.cfg.readOnly, s.cfg.mode, roots,
		limits{s.cfg.maxChunk, s.cfg.maxUpload, s.cfg.maxProject, maxOpen},
		versions{1, s.cfg.version, s.cfg.upstream},
	})
}

var digitsRE = regexp.MustCompile(`^[0-9]{1,18}$`)

func (s *server) apiList(w http.ResponseWriter, r *http.Request) {
	q, e := query(r)
	var vpath string
	if e == nil {
		vpath, e = pathParam(q)
	}
	limit, skip := 500, 0
	if e == nil && q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || !digitsRE.MatchString(q.Get("limit")) || n < 1 || n > 2000 {
			e = badRequest("limit must be 1 to 2000")
		}
		limit = n
	}
	if c := q.Get("cursor"); e == nil && c != "" {
		if !digitsRE.MatchString(c) {
			e = badRequest("invalid cursor")
		}
		skip, _ = strconv.Atoi(c)
	}
	if e != nil {
		writeError(w, r, e)
		return
	}
	rt, dir, rel, e := s.lookup(vpath)
	if e != nil {
		writeError(w, r, e)
		return
	}
	defer dir.Close()
	f, fi, e := openNode(dir, rel)
	if e != nil {
		writeError(w, r, e)
		return
	}
	defer f.Close()
	if !fi.IsDir() {
		writeError(w, r, &apiError{status: http.StatusBadRequest, code: "not_a_directory", msg: "not a directory"})
		return
	}
	names, err := f.Readdirnames(-1)
	if err != nil {
		writeError(w, r, fsError(err))
		return
	}
	entries := make([]entry, 0, len(names))
	for _, name := range names {
		// A name that is not a valid segment could not be addressed anyway.
		if !validName(name) {
			continue
		}
		fi, err := dir.Lstat(path.Join(rel, name))
		if err != nil {
			continue
		}
		if en, ok := newEntry(name, fi); ok {
			entries = append(entries, en)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Type != b.Type {
			return a.Type == "dir"
		}
		if la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name); la != lb {
			return la < lb
		}
		return a.Name < b.Name
	})
	next := ""
	skip = min(skip, len(entries))
	end := len(entries)
	if skip+limit < end {
		end = skip + limit
		next = strconv.Itoa(end)
	}
	writeJSON(w, r, http.StatusOK, struct {
		Path       string  `json:"path"`
		Root       string  `json:"root"`
		Writable   bool    `json:"writable"`
		Entries    []entry `json:"entries"`
		NextCursor string  `json:"nextCursor"`
	}{vpath, rt.id, s.writable(rt), entries[skip:end], next})
}

func (s *server) apiStat(w http.ResponseWriter, r *http.Request) {
	q, e := query(r)
	var vpath string
	if e == nil {
		vpath, e = pathParam(q)
	}
	if e != nil {
		writeError(w, r, e)
		return
	}
	rt, dir, rel, e := s.lookup(vpath)
	if e != nil {
		writeError(w, r, e)
		return
	}
	defer dir.Close()
	fi, err := dir.Lstat(rel)
	if err != nil {
		writeError(w, r, fsError(err))
		return
	}
	en, ok := newEntry(path.Base(vpath), fi)
	if !ok {
		writeError(w, r, errNotFound)
		return
	}
	writeJSON(w, r, http.StatusOK, struct {
		Path     string `json:"path"`
		Root     string `json:"root"`
		Writable bool   `json:"writable"`
		Entry    entry  `json:"entry"`
	}{vpath, rt.id, s.writable(rt), en})
}

func (s *server) apiMkdir(w http.ResponseWriter, r *http.Request) {
	const maxBody = 8192
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	var req struct {
		Path *string `json:"path"`
	}
	if err != nil || len(body) > maxBody || json.Unmarshal(body, &req) != nil || req.Path == nil {
		writeError(w, r, badRequest(`body must be a JSON object with a string "path"`))
		return
	}
	vpath := *req.Path
	rt, dir, rel, e := s.lookup(vpath)
	if e != nil {
		writeError(w, r, e)
		return
	}
	defer dir.Close()
	if !s.writable(rt) {
		writeError(w, r, errReadOnly)
		return
	}
	if rel == "." {
		writeError(w, r, errExists)
		return
	}
	if err := dir.Mkdir(rel, 0o755); err != nil {
		e := fsError(err)
		if errors.Is(err, fs.ErrExist) {
			e = errExists
		}
		writeError(w, r, e)
		return
	}
	fi, err := dir.Lstat(rel)
	if err != nil {
		writeError(w, r, fsError(err))
		return
	}
	en, _ := newEntry(path.Base(vpath), fi)
	writeJSON(w, r, http.StatusCreated, struct {
		Path  string `json:"path"`
		Entry entry  `json:"entry"`
	}{vpath, en})
}
