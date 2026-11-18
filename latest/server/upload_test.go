// Copyright The MESA RustGIS App authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func (e *env) put(query string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do("PUT", "/api/v1/upload"+query, body, append([]string{"X-RGIS-Host", "1", "Content-Type", "application/octet-stream"}, hdr...)...)
}

func (e *env) uploadOffset(vpath, id string) string {
	e.t.Helper()
	w := e.do("HEAD", "/api/v1/upload"+q("path", vpath, "id", id), nil)
	wantStatus(e.t, w, 200)
	if w.Body.Len() != 0 {
		e.t.Errorf("HEAD body %q", w.Body.String())
	}
	return w.Header().Get("Upload-Offset")
}

type uploadBody struct {
	Offset    int64  `json:"offset"`
	Committed bool   `json:"committed"`
	Path      string `json:"path"`
	Entry     *entry `json:"entry"`
}

func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func tmpName(dir, id string) string { return dir + "/.rgis-upload-" + id + ".part" }

func TestUploadHappyPath(t *testing.T) {
	e := newEnv(t)
	data := payload(1000)
	vpath := home + "/sub/up load+é.bin"
	if got := e.uploadOffset(vpath, testID); got != "0" {
		t.Fatalf("initial Upload-Offset %q", got)
	}

	w := e.put(q("path", vpath, "id", testID, "offset", "0"), data[:400])
	wantStatus(t, w, 200)
	if strings.TrimSpace(w.Body.String()) != `{"offset":400,"committed":false}` {
		t.Errorf("chunk response %s", w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", w.Header())
	}
	tmp := tmpName(e.ds+home+"/sub", testID)
	if fi, err := os.Stat(tmp); err != nil || fi.Size() != 400 || fi.Mode().Perm() != 0o644 {
		t.Fatalf("temp file: %v %v", fi, err)
	}
	if got := strings.Join(listNames(t, e, home+"/sub"), " "); got != "inner.csv" {
		t.Errorf("listing shows upload state: %q", got)
	}
	wantErr(t, e.get("/fs"+home+"/sub/.rgis-upload-"+testID+".part"), 400, "bad_path")
	wantErr(t, e.get("/fs"+vpathEscape(vpath)), 404, "not_found")

	wantStatus(t, e.put(q("path", vpath, "id", testID, "offset", "400"), data[400:800]), 200)
	if got := e.uploadOffset(vpath, testID); got != "800" {
		t.Errorf("Upload-Offset %q", got)
	}
	w = e.put(q("path", vpath, "id", testID, "offset", "800", "commit", "1", "size", "1000"), data[800:])
	wantStatus(t, w, 201)
	u := decode[uploadBody](t, w)
	if u.Offset != 1000 || !u.Committed || u.Path != vpath || u.Entry == nil ||
		u.Entry.Name != "up load+é.bin" || u.Entry.Type != "file" || u.Entry.Size != 1000 || u.Entry.MTime == "" {
		t.Errorf("commit response %s", w.Body.String())
	}
	if !bytes.Equal(mustRead(t, e.ds+vpath), data) {
		t.Error("committed content differs")
	}
	if _, err := os.Lstat(tmp); err == nil {
		t.Error("temp file left behind")
	}
	if got := e.uploadOffset(vpath, testID); got != "0" {
		t.Errorf("Upload-Offset after commit %q", got)
	}
	got := e.get("/fs" + vpathEscape(vpath))
	wantStatus(t, got, 200)
	if sha256.Sum256(got.Body.Bytes()) != sha256.Sum256(data) {
		t.Error("download differs from upload")
	}
}

func vpathEscape(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = strings.ReplaceAll(q("k", s)[3:], "+", "%20")
	}
	return strings.Join(segs, "/")
}

func TestUploadSingleRequestAndEmpty(t *testing.T) {
	e := newEnv(t)
	wantStatus(t, e.put(q("path", "/output/one.bin", "id", testID, "offset", "0", "commit", "1", "size", "5"), []byte("hello")), 201)
	if string(mustRead(t, e.ds+"/output/one.bin")) != "hello" {
		t.Error("content")
	}
	w := e.put(q("path", "/scratch/empty.bin", "id", testID, "offset", "0", "commit", "1", "size", "0"), []byte{})
	wantStatus(t, w, 201)
	if u := decode[uploadBody](t, w); u.Offset != 0 || !u.Committed || u.Entry.Size != 0 {
		t.Errorf("empty commit %s", w.Body.String())
	}
	if fi, err := os.Stat(e.scratch + "/empty.bin"); err != nil || fi.Size() != 0 {
		t.Errorf("empty file: %v", err)
	}

	// An empty commit finishes what is already there.
	vpath := home + "/late.bin"
	wantStatus(t, e.put(q("path", vpath, "id", testID, "offset", "0"), []byte("abc")), 200)
	wantStatus(t, e.put(q("path", vpath, "id", testID, "offset", "3", "commit", "1", "size", "3"), nil), 201)
	if string(mustRead(t, e.ds+vpath)) != "abc" {
		t.Error("late commit content")
	}
}

func TestUploadOffsetMismatch(t *testing.T) {
	e := newEnv(t)
	vpath := home + "/off.bin"
	check := func(w *httptest.ResponseRecorder, want int64) {
		t.Helper()
		b := wantErr(t, w, 409, "offset_mismatch")
		if b.Offset == nil || *b.Offset != want {
			t.Errorf("offset in body %v, want %d (%s)", b.Offset, want, w.Body.String())
		}
	}
	check(e.put(q("path", vpath, "id", testID, "offset", "5"), []byte("x")), 0)
	if _, err := os.Lstat(tmpName(e.ds+home, testID)); err == nil {
		t.Error("a refused chunk created the temp file")
	}
	wantStatus(t, e.put(q("path", vpath, "id", testID, "offset", "0"), []byte("0123")), 200)
	check(e.put(q("path", vpath, "id", testID, "offset", "0"), []byte("0123")), 4) // replayed chunk
	check(e.put(q("path", vpath, "id", testID, "offset", "9"), []byte("x")), 4)
	check(e.put(q("path", vpath, "id", testID, "offset", "3", "commit", "1", "size", "4"), nil), 4)
	if string(mustRead(t, tmpName(e.ds+home, testID))) != "0123" {
		t.Error("temp file changed by a refused chunk")
	}
	// Another id is another upload.
	other := strings.Repeat("f", 32)
	wantStatus(t, e.put(q("path", vpath, "id", other, "offset", "0"), []byte("zz")), 200)
	if e.uploadOffset(vpath, testID) != "4" || e.uploadOffset(vpath, other) != "2" {
		t.Error("uploads with different ids share state")
	}
}

func TestUploadResumesAfterRestart(t *testing.T) {
	e := newEnv(t)
	data := payload(3000)
	vpath := home + "/resume.bin"
	wantStatus(t, e.put(q("path", vpath, "id", testID, "offset", "0"), data[:1000]), 200)

	e.restart()
	if got := e.uploadOffset(vpath, testID); got != "1000" {
		t.Fatalf("Upload-Offset after restart %q", got)
	}
	b := wantErr(t, e.put(q("path", vpath, "id", testID, "offset", "0"), data[:1000]), 409, "offset_mismatch")
	if b.Offset == nil || *b.Offset != 1000 {
		t.Fatalf("offset after restart %v", b.Offset)
	}
	wantStatus(t, e.put(q("path", vpath, "id", testID, "offset", "1000"), data[1000:2000]), 200)

	e.restart()
	wantStatus(t, e.put(q("path", vpath, "id", testID, "offset", "2000", "commit", "1", "size", "3000"), data[2000:]), 201)
	if !bytes.Equal(mustRead(t, e.ds+vpath), data) {
		t.Error("resumed content differs")
	}
}

func TestUploadOverwrite(t *testing.T) {
	e := newEnv(t)
	vpath := home + "/dem.tif"
	args := []string{"path", vpath, "id", testID, "offset", "0", "commit", "1", "size", "3"}
	wantErr(t, e.put(q(args...), []byte("new")), 409, "exists")
	if string(mustRead(t, e.ds+vpath)) != string(demBytes) {
		t.Fatal("refused upload changed the file")
	}
	if _, err := os.Lstat(tmpName(e.ds+home, testID)); err == nil {
		t.Error("refused upload wrote a temp file")
	}
	for _, bad := range []string{"0", "true", ""} {
		wantErr(t, e.put(q(append(args, "overwrite", bad)...), []byte("new")), 400, "bad_request")
	}
	wantStatus(t, e.put(q(append(args, "overwrite", "1")...), []byte("new")), 201)
	if string(mustRead(t, e.ds+vpath)) != "new" {
		t.Error("overwrite did not replace the file")
	}

	// A directory or a symlink at the final name is never replaced.
	wantErr(t, e.put(q("path", home+"/sub", "id", testID, "offset", "0", "commit", "1", "size", "1", "overwrite", "1"), []byte("x")), 409, "exists")
	wantErr(t, e.put(q("path", home+"/sub", "id", testID, "offset", "0"), []byte("x")), 409, "exists")
	os.Symlink("notes.txt", e.ds+home+"/alias.txt")
	wantErr(t, e.put(q("path", home+"/alias.txt", "id", testID, "offset", "0", "commit", "1", "size", "1", "overwrite", "1"), []byte("x")), 409, "exists")
	if string(mustRead(t, e.ds+home+"/notes.txt")) != "hello\n" {
		t.Error("wrote through a symlink")
	}

	// The file appears while an upload is in flight: the next chunk is refused.
	late := home + "/late.bin"
	wantStatus(t, e.put(q("path", late, "id", testID, "offset", "0"), []byte("ab")), 200)
	e.write(e.ds+late, []byte("theirs"))
	wantErr(t, e.put(q("path", late, "id", testID, "offset", "2", "commit", "1", "size", "2"), nil), 409, "exists")
	wantStatus(t, e.put(q("path", late, "id", testID, "offset", "2", "commit", "1", "size", "2", "overwrite", "1"), nil), 201)
	if string(mustRead(t, e.ds+late)) != "ab" {
		t.Error("late overwrite")
	}
}

// heldPut starts a PUT and sends first, then keeps the body open: by the time
// it returns the handler is past its checks and waiting for more. finish
// sends the rest and returns the response.
func (e *env) heldPut(query string, first []byte) (finish func(rest []byte) *httptest.ResponseRecorder) {
	pr, pw := io.Pipe()
	r := httptest.NewRequest("PUT", "/api/v1/upload"+query, pr)
	r.Host = testHost
	r.Header.Set("X-RGIS-Host", "1")
	r.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.h.ServeHTTP(w, r)
	}()
	pw.Write(first)
	return func(rest []byte) *httptest.ResponseRecorder {
		pw.Write(rest)
		pw.Close()
		<-done
		return w
	}
}

// The final name is checked before the body is read, and the body can take
// minutes. Whatever took the name meanwhile must survive the commit.
func TestUploadCommitRechecksFinalName(t *testing.T) {
	e := newEnv(t)
	id := func(n int) string { return fmt.Sprintf("%032x", n) }
	commit := func(vpath, id string, size int, more ...string) string {
		return q(append([]string{"path", vpath, "id", id, "offset", "0", "commit", "1", "size", fmt.Sprint(size)}, more...)...)
	}

	// A directory is created during an overwrite=1 upload.
	dir := home + "/victimdir"
	finish := e.heldPut(commit(dir, id(1), 4, "overwrite", "1"), []byte("AB"))
	wantStatus(t, e.mkdir(dir), 201)
	wantErr(t, finish([]byte("CD")), 409, "exists")
	if fi, err := os.Lstat(e.ds + dir); err != nil || !fi.IsDir() {
		t.Errorf("the directory was replaced: %v %v", fi, err)
	}

	// Another upload commits the same new name first; neither asked to overwrite.
	file := home + "/report.txt"
	finish = e.heldPut(commit(file, id(2), 8), []byte("ATTA"))
	wantStatus(t, e.put(commit(file, id(3), 8), []byte("ORIGINAL")), 201)
	wantErr(t, finish([]byte("CKER")), 409, "exists")
	if got := string(mustRead(t, e.ds+file)); got != "ORIGINAL" {
		t.Errorf("a commit without overwrite replaced the file: %q", got)
	}
	// The refused upload is complete on disk; it commits once the user agrees.
	if got := e.uploadOffset(file, id(2)); got != "8" {
		t.Errorf("Upload-Offset after the refused commit: %q", got)
	}
	wantStatus(t, e.put(q("path", file, "id", id(2), "offset", "8", "commit", "1", "size", "8", "overwrite", "1"), nil), 201)
	if got := string(mustRead(t, e.ds+file)); got != "ATTACKER" {
		t.Errorf("after overwrite: %q", got)
	}

	// A symlink takes the name during an overwrite=1 upload.
	link := home + "/alias.txt"
	finish = e.heldPut(commit(link, id(4), 2, "overwrite", "1"), []byte("x"))
	if err := os.Symlink("notes.txt", e.ds+link); err != nil {
		t.Fatal(err)
	}
	wantErr(t, finish([]byte("y")), 409, "exists")
	if fi, err := os.Lstat(e.ds + link); err != nil || fi.Mode()&os.ModeSymlink == 0 || string(mustRead(t, e.ds+home+"/notes.txt")) != "hello\n" {
		t.Error("the symlink was replaced or written through")
	}

	// A commit leaves one name behind, with or without overwrite.
	for _, more := range [][]string{nil, {"overwrite", "1"}} {
		wantStatus(t, e.put(commit(home+"/once.bin", id(5), 1, more...), []byte("z")), 201)
		if _, err := os.Lstat(tmpName(e.ds+home, id(5))); err == nil {
			t.Errorf("temp file left after a commit (%v)", more)
		}
	}
}

func TestUploadTempSymlinkIsNotFollowed(t *testing.T) {
	e := newEnv(t)
	os.Symlink("notes.txt", tmpName(e.ds+home, testID))
	w := e.put(q("path", home+"/x.bin", "id", testID, "offset", "0"), []byte("x"))
	if w.Code/100 == 2 {
		t.Errorf("status %d", w.Code)
	}
	if string(mustRead(t, e.ds+home+"/notes.txt")) != "hello\n" {
		t.Error("appended through a symlinked temp file")
	}
}

func TestUploadAbort(t *testing.T) {
	e := newEnv(t)
	vpath := home + "/abort.bin"
	del := func(query string, hdr ...string) *httptest.ResponseRecorder {
		return e.do("DELETE", "/api/v1/upload"+query, nil, append([]string{"X-RGIS-Host", "1"}, hdr...)...)
	}
	wantStatus(t, e.put(q("path", vpath, "id", testID, "offset", "0"), []byte("abcdef")), 200)
	w := del(q("path", vpath, "id", testID))
	wantStatus(t, w, 204)
	if w.Body.Len() != 0 {
		t.Errorf("204 body %q", w.Body.String())
	}
	if _, err := os.Lstat(tmpName(e.ds+home, testID)); err == nil {
		t.Error("temp file survived the abort")
	}
	if got := e.uploadOffset(vpath, testID); got != "0" {
		t.Errorf("Upload-Offset after abort %q", got)
	}
	wantStatus(t, del(q("path", vpath, "id", testID)), 204)
	wantStatus(t, e.put(q("path", vpath, "id", testID, "offset", "0", "commit", "1", "size", "1"), []byte("z")), 201)

	// Abort removes nothing but the temp file of that id.
	wantStatus(t, del(q("path", vpath, "id", testID)), 204)
	wantStatus(t, del(q("path", home+"/dem.tif", "id", testID)), 204)
	wantStatus(t, del(q("path", home+"/sub", "id", testID)), 204)
	if got := strings.Join(listNames(t, e, home), " "); got != "sub abort.bin dem.tif notes.txt" {
		t.Errorf("after aborts: %q", got)
	}
	wantErr(t, del(q("path", home+"/x.bin", "id", "../../../etc/passwd")), 400, "bad_request")
	wantErr(t, del(q("path", home+"/x.bin")), 400, "bad_request")
	wantErr(t, del(q("path", "/iplant/home/shared/x.bin", "id", testID)), 403, "read_only")
	wantErr(t, del(q("path", home+"/nodir/x.bin", "id", testID)), 404, "not_found")
	wantErr(t, del(q("path", "/nowhere/x.bin", "id", testID)), 404, "not_found")
}

func TestUploadTooLarge(t *testing.T) {
	e := newEnv(t, "RGIS_MAX_CHUNK", "262144", "RGIS_MAX_UPLOAD", "300000")
	vpath := home + "/big.bin"
	b := wantErr(t, e.put(q("path", vpath, "id", testID, "offset", "0"), payload(262145)), 413, "chunk_too_large")
	if b.Offset == nil || *b.Offset != 0 {
		t.Errorf("offset %v", b.Offset)
	}
	if _, err := os.Lstat(tmpName(e.ds+home, testID)); err == nil {
		t.Error("an oversized chunk was stored")
	}
	wantStatus(t, e.put(q("path", vpath, "id", testID, "offset", "0"), payload(262144)), 200)
	b = wantErr(t, e.put(q("path", vpath, "id", testID, "offset", "262144"), payload(262145)), 413, "chunk_too_large")
	if b.Offset == nil || *b.Offset != 262144 {
		t.Errorf("offset %v", b.Offset)
	}

	// offset + Content-Length, or the declared size, over RGIS_MAX_UPLOAD.
	b = wantErr(t, e.put(q("path", vpath, "id", testID, "offset", "262144"), payload(300000-262144+1)), 413, "file_too_large")
	if b.Offset != nil {
		t.Errorf("file_too_large carries offset %d", *b.Offset)
	}
	wantErr(t, e.put(q("path", vpath, "id", testID, "offset", "262144", "commit", "1", "size", "300001"), []byte("x")), 413, "file_too_large")
	if got := e.uploadOffset(vpath, testID); got != "262144" {
		t.Errorf("Upload-Offset %q", got)
	}
	wantStatus(t, e.put(q("path", vpath, "id", testID, "offset", "262144", "commit", "1", "size", "300000"), payload(300000-262144)), 201)
}

// A body of undeclared length that runs past the limits is cut there; what
// was written stays and the response reports it.
func TestUploadChunkedBodyOverLimit(t *testing.T) {
	e := newEnv(t, "RGIS_MAX_CHUNK", "262144")
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	send := func(query string, n int) (int, errBody) {
		t.Helper()
		// Hiding the reader's type makes the client send it chunked.
		req, _ := http.NewRequest("PUT", srv.URL+"/api/v1/upload"+query, struct{ io.Reader }{bytes.NewReader(payload(n))})
		req.Header.Set("X-RGIS-Host", "1")
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b errBody
		json.NewDecoder(resp.Body).Decode(&b)
		return resp.StatusCode, b
	}
	vpath := home + "/stream.bin"
	status, b := send(q("path", vpath, "id", testID, "offset", "0"), 262144+100)
	if status != 413 || b.Error.Code != "chunk_too_large" || b.Offset == nil || *b.Offset != 262144 {
		t.Fatalf("status %d, body %+v", status, b)
	}
	if got := e.uploadOffset(vpath, testID); got != "262144" {
		t.Errorf("Upload-Offset %q", got)
	}
	if !bytes.Equal(mustRead(t, tmpName(e.ds+home, testID)), payload(262144 + 100)[:262144]) {
		t.Error("kept bytes are not the start of the body")
	}
	if status, _ := send(q("path", vpath, "id", testID, "offset", "262144"), 1000); status != 200 {
		t.Errorf("chunked body within the limit: %d", status)
	}

	e = newEnv(t, "RGIS_MAX_CHUNK", "262144", "RGIS_MAX_UPLOAD", "1000")
	srv2 := httptest.NewServer(e.h)
	defer srv2.Close()
	srv = srv2
	status, b = send(q("path", vpath, "id", testID, "offset", "0"), 1500)
	if status != 413 || b.Error.Code != "file_too_large" {
		t.Fatalf("status %d, body %+v", status, b)
	}
	if fi, err := os.Stat(tmpName(e.ds+home, testID)); err != nil || fi.Size() > 1000 {
		t.Errorf("temp file past RGIS_MAX_UPLOAD: %v", err)
	}
}

func TestUploadSizeMismatch(t *testing.T) {
	e := newEnv(t)
	vpath := home + "/size.bin"
	for _, size := range []string{"3", "5"} {
		e.do("DELETE", "/api/v1/upload"+q("path", vpath, "id", testID), nil, "X-RGIS-Host", "1")
		b := wantErr(t, e.put(q("path", vpath, "id", testID, "offset", "0", "commit", "1", "size", size), []byte("abcd")), 409, "size_mismatch")
		if b.Offset == nil || *b.Offset != 4 {
			t.Errorf("size=%s: offset %v", size, b.Offset)
		}
		if _, err := os.Stat(e.ds + vpath); err == nil {
			t.Fatal("a mismatched upload was committed")
		}
		if got := e.uploadOffset(vpath, testID); got != "4" {
			t.Errorf("temp file did not stay: %q", got)
		}
	}
	wantStatus(t, e.put(q("path", vpath, "id", testID, "offset", "4", "commit", "1", "size", "4"), nil), 201)
}

func TestUploadParametersAndRoots(t *testing.T) {
	e := newEnv(t)
	ok := map[string]string{"path": home + "/p.bin", "id": testID, "offset": "0"}
	with := func(kv ...string) string {
		m := map[string]string{}
		for k, v := range ok {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			if kv[i+1] == "-" {
				delete(m, kv[i])
			} else {
				m[kv[i]] = kv[i+1]
			}
		}
		var flat []string
		for k, v := range m {
			flat = append(flat, k, v)
		}
		return q(flat...)
	}
	for _, query := range []string{
		with("id", "-"), with("id", ""), with("id", "abc"), with("id", strings.ToUpper(testID)),
		with("id", testID+"0"), with("id", strings.Repeat("g", 32)), with("id", "../"+testID[3:]),
		with("offset", "-"), with("offset", ""), with("offset", "-1"), with("offset", "1.5"), with("offset", "0x10"), with("offset", "+1"),
		with("offset", "99999999999999999999"),
		with("commit", "0"), with("commit", "true"), with("commit", ""),
		with("commit", "1"), with("commit", "1", "size", "x"), with("commit", "1", "size", "-1"),
		with("path", "-"), with("path", home), with("path", "/output"), with("path", "/scratch"),
		with() + "&offset=0", with() + "&id=" + testID, "?%zz",
	} {
		wantErr(t, e.put(query, []byte("x")), 400, "bad_request")
	}
	// size is ignored without commit.
	wantStatus(t, e.put(with("size", "banana"), []byte("x")), 200)

	wantErr(t, e.put(with("path", "/nowhere/p.bin"), []byte("x")), 404, "not_found")
	wantErr(t, e.put(with("path", "/iplant/home/bob/p.bin"), []byte("x")), 404, "not_found")
	wantErr(t, e.put(with("path", home+"/nodir/p.bin"), []byte("x")), 404, "not_found")
	wantErr(t, e.put(with("path", home+"/dem.tif/p.bin"), []byte("x")), 404, "not_found")
	wantErr(t, e.put(with("path", "/iplant/home/shared/p.bin"), []byte("x")), 403, "read_only")
	wantErr(t, e.put(with("path", "/input/p.bin"), []byte("x")), 403, "read_only")
	// The writable check comes before the parent lookup.
	wantErr(t, e.put(with("path", "/iplant/home/shared/nodir/p.bin"), []byte("x")), 403, "read_only")
	if got := strings.Join(listNames(t, e, "/iplant/home/shared"), " "); got != "pub.txt" {
		t.Errorf("shared changed: %q", got)
	}
	if entries, _ := os.ReadDir(e.ds + "/iplant/home/shared"); len(entries) != 1 {
		t.Error("temp file written to a read-only root")
	}

	// HEAD runs the same checks without the writable one.
	head := func(query string) int { return e.do("HEAD", "/api/v1/upload"+query, nil).Code }
	for query, want := range map[string]int{
		q("path", "/iplant/home/shared/p.bin", "id", testID): 200,
		q("path", home+"/p.bin", "id", "nope"):               400,
		q("path", home+"/../p.bin", "id", testID):            400,
		q("path", home, "id", testID):                        400,
		q("path", home+"/nodir/p.bin", "id", testID):         404,
		q("path", "/nowhere/p.bin", "id", testID):            404,
	} {
		if got := head(query); got != want {
			t.Errorf("HEAD %s: %d, want %d", query, got, want)
		}
	}
}

func TestUploadConcurrentDuplicate(t *testing.T) {
	e := newEnv(t)
	vpath := home + "/race.bin"
	chunk := payload(64 << 10)
	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = e.put(q("path", vpath, "id", testID, "offset", "0"), chunk).Code
		}()
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case 200:
			ok++
		case 409:
			conflict++
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Errorf("statuses %v", codes)
	}
	if got := e.uploadOffset(vpath, testID); got != fmt.Sprint(len(chunk)) {
		t.Errorf("Upload-Offset %q", got)
	}
	s := newServer(&config{})
	if s.locks.lock("k")(); len(s.locks.m) != 0 {
		t.Error("idle lock entry kept")
	}
}

// An early refusal must still leave the connection usable: the body is
// drained, so the client reads the status instead of a reset.
func TestUploadEarlyRefusalOverHTTP(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	body := payload(4 << 20)
	for _, tc := range []struct {
		query, header string
		status        int
	}{
		{q("path", home+"/dem.tif", "id", testID, "offset", "0"), "1", 409},
		{q("path", home+"/e.bin", "id", testID, "offset", "7"), "1", 409},
		{q("path", home+"/e.bin", "id", "bad", "offset", "0"), "1", 400},
		{q("path", "/iplant/home/shared/e.bin", "id", testID, "offset", "0"), "1", 403},
		{q("path", home+"/e.bin", "id", testID, "offset", "0"), "", 403},
	} {
		req, _ := http.NewRequest("PUT", srv.URL+"/api/v1/upload"+tc.query, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/octet-stream")
		if tc.header != "" {
			req.Header.Set("X-RGIS-Host", tc.header)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("%s: %d, want %d", tc.query, resp.StatusCode, tc.status)
		}
	}
}
