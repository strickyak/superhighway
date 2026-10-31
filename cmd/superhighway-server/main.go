// Package main implements a lightweight HTTP read/write filesystem server.
//
// The server exposes a local directory (whose flag path must end with "/fs/")
// as the root "/" over HTTP using standard and bespoke HTTP methods:
//
//   - GET /path/to/file      : Download file contents (or list dir if path is a directory)
//   - HEAD /path/to/file     : Get file headers (Content-Length, Last-Modified)
//   - PUT /path/to/file      : Create or overwrite file with request body
//   - RM /path/to/file       : Remove a file (alias: DELETE)
//   - DELETE /path/to/file   : Remove a file (alias: RM)
//   - MKDIR /path/to/dir     : Create a directory (alias: MKCOL; ?p=1 for MkdirAll)
//   - RMDIR /path/to/dir     : Remove an empty directory
//   - LS /path               : List directory or file info (alias: LIST)
//   - DU /path               : Compute recursive disk usage in bytes for files under path
//   - TIME /                 : Return current UTC time ("YYYY-mm-dd HH:MM:SS <unix_seconds>\n")
//
// Access requires HTTP Basic Authentication with any short ASCII username
// (1..32 printable ASCII characters) and the password specified by --password.
// Authentication failures return "418 I'm a teapot" and are not logged.
// Changes made to the site (PUT, RM/DELETE, MKDIR/MKCOL, RMDIR) are audit-logged
// to stderr with timestamp, client IP, username, method, and path.
package main

import (
	"crypto/subtle"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
)

var (
	flagFS       = flag.String("fs", "./fs/", `path to served directory; MUST end with "/fs/" (e.g. "./fs/" or "dirname/fs/")`)
	flagAddr     = flag.String("addr", ":8080", "HTTP listen address")
	flagPassword = flag.String("password", "", "required Basic Auth password")
)

const timeFormat = "2006-01-02 15:04:05"

// ValidateFSFlag ensures the --fs flag value ends with "/fs/" for safety.
func ValidateFSFlag(fsPath string) error {
	if !strings.HasSuffix(fsPath, "/fs/") {
		return fmt.Errorf("invalid --fs=%q: must end with \"/fs/\" (e.g. \"./fs/\" or \"dirname/fs/\")", fsPath)
	}
	return nil
}

// SanitizeUsername removes any bytes outside printable ASCII 32..126.
func SanitizeUsername(username string) string {
	var b strings.Builder
	b.Grow(len(username))
	for i := 0; i < len(username); i++ {
		c := username[i]
		if c >= 32 && c <= 126 {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ValidateUsername checks that the sanitized username is 1..32 ASCII characters (32..126).
func ValidateUsername(username string) bool {
	u := SanitizeUsername(username)
	return len(u) >= 1 && len(u) <= 32
}

// Server serves a rooted filesystem over HTTP.
type Server struct {
	root        *os.Root
	password    string
	auditMu     sync.Mutex
	auditWriter io.Writer
}

// NewServer validates fsPath and password, and opens an os.Root confined to that directory.
func NewServer(fsPath, password string) (*Server, error) {
	if err := ValidateFSFlag(fsPath); err != nil {
		return nil, err
	}
	if password == "" {
		return nil, errors.New("--password must be non-empty")
	}
	info, err := os.Stat(fsPath)
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", fsPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%q is not a directory", fsPath)
	}
	root, err := os.OpenRoot(fsPath)
	if err != nil {
		return nil, fmt.Errorf("open root %q: %w", fsPath, err)
	}
	return &Server{
		root:        root,
		password:    password,
		auditWriter: os.Stderr,
	}, nil
}

// Close releases the underlying os.Root handle.
func (s *Server) Close() error {
	if s.root != nil {
		return s.root.Close()
	}
	return nil
}

func (s *Server) authenticate(r *http.Request) (string, bool) {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return "", false
	}
	user = SanitizeUsername(user)
	if len(user) < 1 || len(user) > 32 {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(pass), []byte(s.password)) != 1 {
		return "", false
	}
	return user, true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

func (s *Server) logChange(r *http.Request, user, action, rel string) {
	ts := time.Now().UTC().Format(timeFormat)
	ip := clientIP(r)
	cleanPath := "/" + rel
	if rel == "." {
		cleanPath = "/"
	}
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	cw := csv.NewWriter(s.auditWriter)
	_ = cw.Write([]string{"csv", ts, ip, user, action, cleanPath})
	cw.Flush()
}

// resolveRelPath validates and converts a request URL path to a relative path
// suitable for os.Root methods ("." for root).
func resolveRelPath(urlPath string) (string, error) {
	if strings.ContainsRune(urlPath, '\x00') {
		return "", errors.New("invalid null byte in path")
	}
	for _, seg := range strings.Split(urlPath, "/") {
		if seg == ".." {
			return "", errors.New("path traversal \"..\" not allowed")
		}
	}
	cleaned := path.Clean("/" + urlPath)
	rel := strings.TrimPrefix(cleaned, "/")
	if rel == "" {
		return ".", nil
	}
	return rel, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(r)
	if !ok {
		http.Error(w, "418 I'm a teapot", http.StatusTeapot)
		return
	}

	rel, err := resolveRelPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch strings.ToUpper(r.Method) {
	case http.MethodGet, http.MethodHead:
		s.handleGetOrHead(w, r, rel)
	case "LS", "LIST":
		s.handleLS(w, r, rel)
	case http.MethodPut:
		s.handlePut(w, r, user, rel)
	case "RM", http.MethodDelete:
		s.handleRM(w, r, user, rel)
	case "MKDIR", "MKCOL":
		s.handleMkdir(w, r, user, rel)
	case "RMDIR":
		s.handleRmdir(w, r, user, rel)
	case "DU":
		s.handleDU(w, r, rel)
	case "TIME", "NOW", "DATE":
		s.handleTime(w)
	default:
		http.Error(w, fmt.Sprintf("unsupported method %q", r.Method), http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleGetOrHead(w http.ResponseWriter, r *http.Request, rel string) {
	info, err := s.root.Stat(rel)
	if err != nil {
		writeFSError(w, err)
		return
	}
	if info.IsDir() {
		s.handleLS(w, r, rel)
		return
	}
	f, err := s.root.Open(rel)
	if err != nil {
		writeFSError(w, err)
		return
	}
	defer f.Close()

	http.ServeContent(w, r, info.Name(), info.ModTime().UTC(), f)
}

func (s *Server) handleLS(w http.ResponseWriter, r *http.Request, rel string) {
	info, err := s.root.Stat(rel)
	if err != nil {
		writeFSError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !info.IsDir() {
		if r.Method == http.MethodHead {
			return
		}
		fmt.Fprint(w, formatLSEntry(info.Name(), info))
		return
	}

	f, err := s.root.Open(rel)
	if err != nil {
		writeFSError(w, err)
		return
	}
	defer f.Close()

	entries, err := f.ReadDir(-1)
	if err != nil {
		writeFSError(w, err)
		return
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})

	if r.Method == http.MethodHead {
		return
	}

	for _, entry := range entries {
		eInfo, err := entry.Info()
		if err != nil {
			continue
		}
		fmt.Fprint(w, formatLSEntry(entry.Name(), eInfo))
	}
}

func formatLSEntry(name string, info fs.FileInfo) string {
	suffix := ""
	if info.IsDir() {
		suffix = "/"
	}
	return fmt.Sprintf("%s %d %s%s\n",
		info.ModTime().UTC().Format(timeFormat),
		info.Size(),
		name,
		suffix,
	)
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, user, rel string) {
	if rel == "." {
		http.Error(w, "cannot PUT to root directory", http.StatusBadRequest)
		return
	}
	if info, err := s.root.Stat(rel); err == nil && info.IsDir() {
		http.Error(w, "target is a directory", http.StatusConflict)
		return
	}

	f, err := s.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		writeFSError(w, err)
		return
	}

	_, copyErr := io.Copy(f, r.Body)
	closeErr := f.Close()
	if copyErr != nil {
		writeFSError(w, copyErr)
		return
	}
	if closeErr != nil {
		writeFSError(w, closeErr)
		return
	}
	s.logChange(r, user, "PUT", rel)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleRM(w http.ResponseWriter, r *http.Request, user, rel string) {
	if rel == "." {
		http.Error(w, "cannot remove root directory", http.StatusBadRequest)
		return
	}
	info, err := s.root.Lstat(rel)
	if err != nil {
		writeFSError(w, err)
		return
	}
	if info.IsDir() {
		http.Error(w, "target is a directory; use RMDIR", http.StatusBadRequest)
		return
	}
	if err := s.root.Remove(rel); err != nil {
		writeFSError(w, err)
		return
	}
	s.logChange(r, user, "RM", rel)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request, user, rel string) {
	if rel == "." {
		http.Error(w, "root directory already exists", http.StatusConflict)
		return
	}
	var err error
	if r.URL.Query().Get("p") == "1" || r.URL.Query().Get("parents") == "1" {
		err = s.root.MkdirAll(rel, 0755)
	} else {
		err = s.root.Mkdir(rel, 0755)
	}
	if err != nil {
		writeFSError(w, err)
		return
	}
	s.logChange(r, user, "MKDIR", rel)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleRmdir(w http.ResponseWriter, r *http.Request, user, rel string) {
	if rel == "." {
		http.Error(w, "cannot remove root directory", http.StatusBadRequest)
		return
	}
	info, err := s.root.Lstat(rel)
	if err != nil {
		writeFSError(w, err)
		return
	}
	if !info.IsDir() {
		http.Error(w, "target is not a directory; use RM or DELETE", http.StatusBadRequest)
		return
	}
	if err := s.root.Remove(rel); err != nil {
		writeFSError(w, err)
		return
	}
	s.logChange(r, user, "RMDIR", rel)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDU(w http.ResponseWriter, _ *http.Request, rel string) {
	if _, err := s.root.Lstat(rel); err != nil {
		writeFSError(w, err)
		return
	}

	var totalBytes int64
	err := fs.WalkDir(s.root.FS(), rel, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			totalBytes += info.Size()
		}
		return nil
	})
	if err != nil {
		writeFSError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "%d\n", totalBytes)
}

func (s *Server) handleTime(w http.ResponseWriter) {
	now := time.Now().UTC()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "%s %d\n", now.Format(timeFormat), now.Unix())
}

func writeFSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, fs.ErrExist):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, fs.ErrPermission):
		http.Error(w, err.Error(), http.StatusForbidden)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

func main() {
	flag.Parse()

	srv, err := NewServer(*flagFS, *flagPassword)
	if err != nil {
		log.Fatalf("Fatal: %v", err)
	}
	defer srv.Close()

	log.Printf("Serving %q as root \"/\" on %s", *flagFS, *flagAddr)
	if err := http.ListenAndServe(*flagAddr, srv); err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}
