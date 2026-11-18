// Copyright The MESA RustGIS App authors
// SPDX-License-Identifier: Apache-2.0

// Command rgis-host serves the RustGIS web bundle and a guarded file API over
// the Data Store mount. The contract is docs/architecture.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxOpen    = 16
	minChunk   = 256 << 10
	drainLimit = 16 << 20
)

type config struct {
	addr, appDir, dataStore, zone, user, scratch string
	readOnly                                     bool
	publicScheme, publicHost                     string
	allowedHosts                                 []string
	maxChunk, maxUpload, maxProject              int64
	version, upstream, mode                      string
}

var userRE = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

func loadConfig(lookup func(string) (string, bool)) (*config, error) {
	get := func(key, def string) string {
		if v, ok := lookup(key); ok && v != "" {
			return v
		}
		return def
	}
	c := &config{
		addr:      get("RGIS_ADDR", "0.0.0.0:8080"),
		appDir:    get("RGIS_APP_DIR", "/opt/rustgis/app"),
		dataStore: get("RGIS_DATA_STORE", "/data-store"),
		zone:      get("RGIS_ZONE", "iplant"),
		scratch:   get("RGIS_SCRATCH", ""),
		version:   get("RGIS_VERSION", "dev"),
		upstream:  get("RGIS_UPSTREAM", "unknown"),
		mode:      get("RGIS_MODE", "launch"),
	}
	if c.mode != "launch" && c.mode != "bridge" {
		return nil, fmt.Errorf("RGIS_MODE: %q is neither launch nor bridge", c.mode)
	}
	if u := get("RGIS_USER", get("IPLANT_USER", "")); userRE.MatchString(u) && u[0] != '.' && u != "shared" {
		c.user = u
	}
	if c.scratch == "" {
		wd, err := os.Getwd()
		if err != nil {
			wd = "."
		}
		c.scratch = filepath.Join(wd, "scratch")
	}
	ro := strings.ToLower(get("RGIS_READONLY", ""))
	c.readOnly = ro == "1" || ro == "true"

	// A set-but-empty RGIS_PUBLIC_URL switches the public URL off, REDIRECT_URL included.
	pub, set := lookup("RGIS_PUBLIC_URL")
	if !set {
		pub = get("REDIRECT_URL", "")
	}
	if u, err := url.Parse(pub); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		c.publicScheme, c.publicHost = u.Scheme, u.Host
	}
	for h := range strings.SplitSeq(get("RGIS_ALLOWED_HOSTS", ""), ",") {
		if h = strings.TrimSpace(h); h != "" {
			c.allowedHosts = append(c.allowedHosts, h)
		}
	}

	for _, n := range []struct {
		key      string
		def, min int64
		dst      *int64
	}{
		{"RGIS_MAX_CHUNK", 8 << 20, minChunk, &c.maxChunk},
		{"RGIS_MAX_UPLOAD", 0, 0, &c.maxUpload},
		{"RGIS_MAX_PROJECT", 256 << 20, 0, &c.maxProject},
	} {
		*n.dst = n.def
		if v := get(n.key, ""); v != "" {
			i, err := strconv.ParseInt(v, 10, 64)
			if err != nil || i < n.min {
				return nil, fmt.Errorf("%s: %q is not an integer of at least %d", n.key, v, n.min)
			}
			*n.dst = i
		}
	}
	return c, nil
}

type root struct {
	id, label, vpath, dir string
	writable              bool
}

type server struct {
	cfg       *config
	roots     []root
	projectMu sync.Mutex
	locks     keyedMutex
}

func newServer(c *config) *server {
	s := &server{cfg: c}
	home := "/" + c.zone + "/home/"
	ds := func(v string) string { return filepath.Join(c.dataStore, filepath.FromSlash(v)) }
	if c.user != "" {
		s.roots = append(s.roots, root{"home", "Home", home + c.user, ds(home + c.user), true})
	}
	s.roots = append(s.roots,
		root{"shared", "Community Data", home + "shared", ds(home + "shared"), false},
		root{"input", "Analysis Inputs", "/input", ds("/input"), false},
		root{"output", "Analysis Outputs", "/output", ds("/output"), true},
		root{"scratch", "Session Scratch", "/scratch", c.scratch, true},
	)
	return s
}

// ServeHTTP is the only router: http.ServeMux would redirect unclean paths.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	p := r.URL.Path
	switch {
	case p == "/fs" || strings.HasPrefix(p, "/fs/"):
		s.serveFS(w, r)
	case p == "/api" || strings.HasPrefix(p, "/api/"):
		s.serveAPI(w, r)
	default:
		s.serveStatic(w, r)
	}
}

// scheme is the public scheme the browser used for this request.
func (s *server) scheme(r *http.Request) string {
	if s.cfg.publicScheme != "" {
		return s.cfg.publicScheme
	}
	first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	if first = strings.TrimSpace(first); first == "https" {
		return first
	}
	return "http"
}

// origin is the public origin the browser used for this request.
func (s *server) origin(r *http.Request) string {
	return s.scheme(r) + "://" + r.Host
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// ReadFrom keeps the sendfile path that http.ServeContent would otherwise lose
// behind the wrapper.
func (w *statusWriter) ReadFrom(src io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := io.Copy(w.ResponseWriter, src)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		log.Printf("%s %q %d %d %s", r.Method, r.URL.Path, sw.status, sw.bytes, time.Since(start).Round(time.Microsecond))
	})
}

func healthcheck(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}

func main() {
	cfg, err := loadConfig(os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rgis-host:", err)
		os.Exit(2)
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck", "-healthcheck", "--healthcheck":
			os.Exit(healthcheck(cfg.addr))
		}
		fmt.Fprintln(os.Stderr, "usage: rgis-host [healthcheck]")
		os.Exit(2)
	}

	if !cfg.readOnly {
		if err := os.MkdirAll(cfg.scratch, 0o755); err != nil {
			log.Printf("scratch directory unavailable: %v", err)
		}
	}
	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           logRequests(newServer(cfg)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Otherwise net/http answers "OPTIONS *" with 200 itself.
		DisableGeneralOptionsHandler: true,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			srv.Close()
		}
	}()
	log.Printf("rgis-host %s (upstream %s, %s mode) listening on %s", cfg.version, cfg.upstream, cfg.mode, cfg.addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Print(err)
		os.Exit(1)
	}
	// ListenAndServe returns as soon as Shutdown starts; wait for it to drain.
	<-done
}
