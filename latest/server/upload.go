// Copyright The MESA RustGIS App authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"sync"
	"syscall"
)

var uploadIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// keyedMutex serialises work per key without keeping idle entries.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyedLock{}
	}
	l := k.m[key]
	if l == nil {
		l = &keyedLock{}
		k.m[key] = l
	}
	l.refs++
	k.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// drain discards part of an unread body so the client gets the status rather
// than a connection reset.
func drain(r *http.Request) {
	io.CopyN(io.Discard, r.Body, drainLimit)
}

type upload struct {
	vpath            string
	rt               *root
	dir              *os.Root
	final, tmp       string // relative to the root
	offset, size     int64
	commit, override bool
}

// openUpload runs the parameter, root and parent checks shared by PUT, HEAD
// and DELETE. On success the caller closes u.dir.
func (s *server) openUpload(r *http.Request) (*upload, *apiError) {
	q, e := query(r)
	if e != nil {
		return nil, e
	}
	u := &upload{}
	if u.vpath, e = pathParam(q); e != nil {
		return nil, e
	}
	id := q.Get("id")
	if !uploadIDRE.MatchString(id) {
		return nil, badRequest("id must be 32 lower-case hex characters")
	}
	mutation := r.Method != http.MethodHead
	if r.Method == http.MethodPut {
		flag := func(key string) (bool, *apiError) {
			if q.Has(key) && q.Get(key) != "1" {
				return false, badRequest(key + " must be absent or 1")
			}
			return q.Has(key), nil
		}
		number := func(key string) (int64, *apiError) {
			if !digitsRE.MatchString(q.Get(key)) {
				return 0, badRequest(key + " must be a decimal number")
			}
			n, _ := strconv.ParseInt(q.Get(key), 10, 64)
			return n, nil
		}
		if u.offset, e = number("offset"); e != nil {
			return nil, e
		}
		if u.commit, e = flag("commit"); e != nil {
			return nil, e
		}
		if u.override, e = flag("overwrite"); e != nil {
			return nil, e
		}
		if u.commit {
			if u.size, e = number("size"); e != nil {
				return nil, e
			}
		}
	}

	rt, rel, ok := s.locate(u.vpath)
	if !ok {
		return nil, errNotFound
	}
	if rel == "." {
		return nil, badRequest("path is a root")
	}
	if mutation && !s.writable(rt) {
		return nil, errReadOnly
	}
	u.rt, u.final = rt, rel
	u.tmp = path.Join(path.Dir(rel), ".rgis-upload-"+id+".part")

	dir, err := os.OpenRoot(rt.dir)
	if err != nil {
		return nil, fsError(err)
	}
	e = plainDirs(dir, rel)
	if e == nil {
		if fi, err := dir.Lstat(path.Dir(rel)); err != nil {
			e = fsError(err)
		} else if !fi.IsDir() {
			e = errNotFound
		}
	}
	if e != nil {
		dir.Close()
		return nil, e
	}
	u.dir = dir
	return u, nil
}

// tmpSize is the temp file's size, 0 when there is none.
func (u *upload) tmpSize() (int64, *apiError) {
	fi, err := u.dir.Lstat(u.tmp)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, nil
	case err != nil:
		return 0, fsError(err)
	case !fi.Mode().IsRegular():
		return 0, &apiError{status: http.StatusInternalServerError, code: "io_error", msg: "upload temp file is not a regular file"}
	}
	return fi.Size(), nil
}

func (s *server) uploadHead(w http.ResponseWriter, r *http.Request) {
	u, e := s.openUpload(r)
	if e != nil {
		writeError(w, r, e)
		return
	}
	defer u.dir.Close()
	size, e := u.tmpSize()
	if e != nil {
		writeError(w, r, e)
		return
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
}

func (s *server) uploadDelete(w http.ResponseWriter, r *http.Request) {
	u, e := s.openUpload(r)
	if e != nil {
		writeError(w, r, e)
		return
	}
	defer u.dir.Close()
	defer s.locks.lock(u.lockKey())()
	if err := u.dir.Remove(u.tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		writeError(w, r, fsError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// The two keys cannot collide: no final name starts with a dot.
func (u *upload) lockKey() string   { return u.rt.id + "/" + u.tmp }
func (u *upload) commitKey() string { return u.rt.id + "/" + u.final }

func (s *server) uploadPut(w http.ResponseWriter, r *http.Request) {
	u, e := s.openUpload(r)
	if e != nil {
		drain(r)
		writeError(w, r, e)
		return
	}
	defer u.dir.Close()
	status, body, e, early := s.appendChunk(u, r)
	if e != nil {
		// The lock is released by now, so a retry is not held up by the drain.
		if early {
			drain(r)
		}
		writeError(w, r, e)
		return
	}
	writeJSON(w, r, status, body)
}

type uploadResult struct {
	Offset    int64  `json:"offset"`
	Committed bool   `json:"committed"`
	Path      string `json:"path,omitempty"`
	Entry     *entry `json:"entry,omitempty"`
}

// bodyReader remembers a read failure so it can be told apart from a write
// failure after io.Copy.
type bodyReader struct {
	r   io.Reader
	err error
}

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && err != io.EOF {
		b.err = err
	}
	return n, err
}

// appendChunk is PUT steps 4 to 8, under the temp file's lock, followed by the
// commit. early reports that the body has not been read.
func (s *server) appendChunk(u *upload, r *http.Request) (status int, res *uploadResult, e *apiError, early bool) {
	defer s.locks.lock(u.lockKey())()

	// Lstat: a symlink at the final name is never replaced or written through.
	if fi, err := u.dir.Lstat(u.final); err == nil {
		if !u.override || !fi.Mode().IsRegular() {
			return 0, nil, errExists, true
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, nil, fsError(err), true
	}
	cur, e := u.tmpSize()
	if e != nil {
		return 0, nil, e, true
	}
	if u.offset != cur {
		return 0, nil, (&apiError{status: http.StatusConflict, code: "offset_mismatch", msg: fmt.Sprintf("expected offset %d", cur)}).withOffset(cur), true
	}
	tooLarge := func(code, what string) *apiError {
		return &apiError{status: http.StatusRequestEntityTooLarge, code: code, msg: what}
	}
	chunkTooLarge := tooLarge("chunk_too_large", fmt.Sprintf("chunk exceeds %d bytes", s.cfg.maxChunk))
	fileTooLarge := tooLarge("file_too_large", fmt.Sprintf("file exceeds %d bytes", s.cfg.maxUpload))
	declared := max(r.ContentLength, 0) // -1 when the length is not declared
	if declared > s.cfg.maxChunk {
		return 0, nil, chunkTooLarge.withOffset(cur), true
	}
	limit, over := s.cfg.maxChunk, chunkTooLarge
	if most := s.cfg.maxUpload; most > 0 {
		if cur+declared > most || (u.commit && u.size > most) {
			return 0, nil, fileTooLarge, true
		}
		// Only a body of undeclared length can still run past the file limit.
		if most-cur < limit {
			limit, over = most-cur, fileTooLarge
		}
	}

	f, err := u.dir.OpenFile(u.tmp, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return 0, nil, fsError(err), true
	}
	src := &bodyReader{r: r.Body}
	_, err = io.Copy(f, io.LimitReader(src, limit))
	overflow := false
	if err == nil {
		var one [1]byte
		n, _ := io.ReadFull(src, one[:])
		overflow = n > 0
	}
	if serr := f.Sync(); err == nil && src.err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil && src.err == nil {
		err = cerr
	}
	size, e := u.tmpSize()
	switch {
	case src.err != nil:
		return 0, nil, badRequest("request body ended early"), false
	case err != nil:
		return 0, nil, fsError(err), false
	case e != nil:
		return 0, nil, e, false
	case overflow:
		return 0, nil, over.withOffset(size), false
	}
	if !u.commit {
		return http.StatusOK, &uploadResult{Offset: size}, nil, false
	}

	if size != u.size {
		return 0, nil, (&apiError{status: http.StatusConflict, code: "size_mismatch", msg: fmt.Sprintf("received %d bytes, expected %d", size, u.size)}).withOffset(size), false
	}
	if e := s.commitUpload(u); e != nil {
		return 0, nil, e, false
	}
	fi, err := u.dir.Lstat(u.final)
	if err != nil {
		return 0, nil, fsError(err), false
	}
	en, _ := newEntry(path.Base(u.vpath), fi)
	return http.StatusCreated, &uploadResult{Offset: size, Committed: true, Path: u.vpath, Entry: &en}, nil, false
}

// commitUpload is PUT step 9: it gives the complete temp file its final name.
// The final name's lock makes the check and the move one step for this
// process, so two uploads cannot both find the name free.
func (s *server) commitUpload(u *upload) *apiError {
	defer s.locks.lock(u.commitKey())()

	// The check in step 4 is as old as the body took to arrive.
	fi, err := u.dir.Lstat(u.final)
	switch {
	case err == nil && (!u.override || !fi.Mode().IsRegular()):
		return errExists
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fsError(err)
	}
	if !u.override {
		// A hard link never replaces, whoever created the name in between.
		// Where the filesystem has no hard links (irodsfs), the check above
		// and the rename below have to do.
		switch err := u.dir.Link(u.tmp, u.final); {
		case err == nil:
			if err := u.dir.Remove(u.tmp); err != nil {
				log.Printf("upload temp file left behind: %v", err)
			}
			return nil
		case errors.Is(err, fs.ErrExist):
			return errExists
		}
	}
	// rename(2) replaces a regular file and refuses a directory.
	if err := u.dir.Rename(u.tmp, u.final); err != nil {
		return fsError(err)
	}
	return nil
}
