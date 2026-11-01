// Package main implements the highway client for syncing files with a
// superhighway-server over HTTP based on file size differences.
//
// Usage:
//
//	go run ./cmd/highway -fs /tmp/fs/ -url 'http://strick:pw123@pizga.net:1908/' -c download
//	go run ./cmd/highway -fs /tmp/fs/ -url 'http://strick:pw123@pizga.net:1908/' -c upload
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	flagFS      = flag.String("fs", "./fs/", `local directory path; MUST end with "/fs/" (e.g. "/tmp/fs/" or "./fs/")`)
	flagURL     = flag.String("url", "", "superhighway server URL (e.g. 'http://user:pass@host:1908/')")
	flagCmd     = flag.String("c", "", "command to execute: 'download' or 'upload'")
	flagAll     = flag.Bool("all", false, "force all files to be overwritten, even if sizes match")
	flagDelete  = flag.Bool("delete", false, "delete files or directories in the destination that are not in the source")
	flagDryRun  = flag.Bool("n", false, "dry-run: do not modify local or remote files (combine with -v to see planned actions)")
	flagVerbose = flag.Bool("v", false, "log every HTTP request URL and action to stderr")
)

const timeFormat = "2006-01-02 15:04:05"

// ValidateFSFlag ensures the -fs flag value ends with "/fs/" for safety.
func ValidateFSFlag(fsPath string) error {
	if !strings.HasSuffix(fsPath, "/fs/") {
		return fmt.Errorf("invalid -fs=%q: must end with \"/fs/\" (e.g. \"/tmp/fs/\" or \"./fs/\")", fsPath)
	}
	return nil
}

// Entry represents a single line from an LS response.
type Entry struct {
	Name    string
	Size    int64
	ModTime time.Time
	IsDir   bool
}

// ParseLSLine parses a single line from the server's LS output:
//
//	YYYY-mm-dd HH:MM:SS 12345 Filename might have spaces and #@%$%? funny chars!
func ParseLSLine(line string) (Entry, error) {
	line = strings.TrimSuffix(line, "\r")
	// Split into at most 4 parts on the first 3 spaces: date, time, size, name
	parts := strings.SplitN(line, " ", 4)
	if len(parts) < 4 {
		return Entry{}, fmt.Errorf("invalid LS line %q", line)
	}
	mtime, err := time.Parse(timeFormat, parts[0]+" "+parts[1])
	if err != nil {
		return Entry{}, fmt.Errorf("invalid timestamp in LS line %q: %w", line, err)
	}
	size, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return Entry{}, fmt.Errorf("invalid size in LS line %q: %w", line, err)
	}
	rawName := parts[3]
	isDir := strings.HasSuffix(rawName, "/")
	name := strings.TrimSuffix(rawName, "/")
	if name == "" {
		return Entry{}, fmt.Errorf("empty name in LS line %q", line)
	}
	return Entry{
		Name:    name,
		Size:    size,
		ModTime: mtime.UTC(),
		IsDir:   isDir,
	}, nil
}

// ParseLS parses a full LS response body into a slice of Entries.
// The response starts with an integer count line, followed by that many
// entry lines, and ends with a line containing only ";".
func ParseLS(body string) ([]Entry, error) {
	body = strings.TrimSuffix(strings.TrimSuffix(body, "\n"), "\r")
	lines := strings.Split(body, "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("invalid LS response: expected at least count and ';' lines, got %d line(s)", len(lines))
	}
	firstLine := strings.TrimSuffix(lines[0], "\r")
	count, err := strconv.Atoi(firstLine)
	if err != nil || count < 0 {
		return nil, fmt.Errorf("invalid LS count line %q", firstLine)
	}
	lastLine := strings.TrimSuffix(lines[len(lines)-1], "\r")
	if lastLine != ";" {
		return nil, fmt.Errorf("invalid LS terminator line %q (want \";\")", lastLine)
	}
	entryLines := lines[1 : len(lines)-1]
	if len(entryLines) != count {
		return nil, fmt.Errorf("LS entry count mismatch: header said %d, got %d", count, len(entryLines))
	}
	entries := make([]Entry, 0, count)
	for _, line := range entryLines {
		e, err := ParseLSLine(line)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// Config holds the configuration for a highway sync operation.
type Config struct {
	FS        string
	URL       string
	Cmd       string
	All       bool
	Delete    bool
	DryRun    bool
	Verbose   bool
	LogWriter io.Writer
}

// Client communicates with a remote superhighway-server.
type Client struct {
	cfg        Config
	baseURL    *url.URL
	username   string
	password   string
	hasAuth    bool
	httpClient *http.Client
}

// NewClient parses cfg.URL (extracting any embedded user:password) and returns a Client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("-url is required")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse url %q: %w", cfg.URL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme %q (want http or https)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("missing host in URL %q", cfg.URL)
	}

	if cfg.LogWriter == nil {
		cfg.LogWriter = os.Stderr
	}
	c := &Client{
		cfg:        cfg,
		httpClient: http.DefaultClient,
	}
	if u.User != nil {
		c.username = u.User.Username()
		c.password, _ = u.User.Password()
		c.hasAuth = true
		u.User = nil
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	c.baseURL = u
	return c, nil
}

// endpointURL builds the full escaped URL for a relative path (where "." is root)
// and optional rawQuery.
func (c *Client) endpointURL(relPath, rawQuery string) string {
	cleanRel := strings.TrimPrefix(path.Clean("/"+relPath), "/")
	fullPath := c.baseURL.Path + "/"
	if cleanRel != "" && cleanRel != "." {
		fullPath += cleanRel
	}
	u := *c.baseURL
	u.Path = fullPath
	u.RawPath = (&url.URL{Path: fullPath}).EscapedPath()
	u.RawQuery = rawQuery
	return u.String()
}

func (c *Client) logf(format string, args ...any) {
	if !c.cfg.Verbose {
		return
	}
	w := c.cfg.LogWriter
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, format, args...)
}

func (c *Client) logRequest(method, relPath, rawQuery string) string {
	target := c.endpointURL(relPath, rawQuery)
	c.logf("%s %s\n", method, target)
	return target
}

func (c *Client) do(method, relPath, rawQuery string, body io.Reader) (*http.Response, error) {
	target := c.logRequest(method, relPath, rawQuery)
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		return nil, err
	}
	if c.hasAuth {
		req.SetBasicAuth(c.username, c.password)
	}
	return c.httpClient.Do(req)
}

// LS lists entries at relPath on the remote server.
// If the remote path does not exist, it returns fs.ErrNotExist.
func (c *Client) LS(relPath string) ([]Entry, error) {
	resp, err := c.do("LS", relPath, "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fs.ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LS %q failed: HTTP %d: %s", relPath, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return ParseLS(string(b))
}

// MkdirAll creates a directory (and any parents) on the remote server.
func (c *Client) MkdirAll(relPath string) error {
	if c.cfg.DryRun {
		c.logRequest("MKDIR", relPath, "p=1")
		return nil
	}
	resp, err := c.do("MKDIR", relPath, "p=1", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("MKDIR %q failed: HTTP %d: %s", relPath, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// Get opens a reader for a remote file at relPath.
func (c *Client) Get(relPath string) (io.ReadCloser, error) {
	resp, err := c.do(http.MethodGet, relPath, "", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("GET %q failed: HTTP %d: %s", relPath, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp.Body, nil
}

// Put uploads the contents of r to relPath on the remote server.
func (c *Client) Put(relPath string, r io.Reader) error {
	if c.cfg.DryRun {
		c.logRequest(http.MethodPut, relPath, "")
		return nil
	}
	resp, err := c.do(http.MethodPut, relPath, "", r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("PUT %q failed: HTTP %d: %s", relPath, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// RM removes a regular file on the remote server.
func (c *Client) RM(relPath string) error {
	if c.cfg.DryRun {
		c.logRequest("RM", relPath, "")
		return nil
	}
	resp, err := c.do("RM", relPath, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("RM %q failed: HTTP %d: %s", relPath, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// Rmdir removes an empty directory on the remote server.
func (c *Client) Rmdir(relPath string) error {
	if c.cfg.DryRun {
		c.logRequest("RMDIR", relPath, "")
		return nil
	}
	resp, err := c.do("RMDIR", relPath, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("RMDIR %q failed: HTTP %d: %s", relPath, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// RemoveAll recursively removes a file or directory on the remote server.
func (c *Client) RemoveAll(relPath string, isDir bool) error {
	if !isDir {
		return c.RM(relPath)
	}
	entries, err := c.LS(relPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		childRel := path.Join(relPath, e.Name)
		if err := c.RemoveAll(childRel, e.IsDir); err != nil {
			return err
		}
	}
	return c.Rmdir(relPath)
}

// SyncDownload recursively downloads files from the remote server into localRoot
// according to c.cfg.
func SyncDownload(c *Client, localRoot *os.Root) error {
	return syncDownloadDir(c, localRoot, ".")
}

func syncDownloadDir(c *Client, localRoot *os.Root, relDir string) error {
	remoteEntries, err := c.LS(relDir)
	if err != nil {
		return err
	}

	if c.cfg.Delete && localRoot != nil {
		remoteMap := make(map[string]Entry, len(remoteEntries))
		for _, e := range remoteEntries {
			remoteMap[e.Name] = e
		}
		if dirFile, err := localRoot.Open(relDir); err == nil {
			localEntries, readErr := dirFile.ReadDir(-1)
			_ = dirFile.Close()
			if readErr != nil {
				return fmt.Errorf("readdir local %q: %w", relDir, readErr)
			}
			for _, d := range localEntries {
				name := d.Name()
				childRel := name
				if relDir != "." {
					childRel = path.Join(relDir, name)
				}
				rem, exists := remoteMap[name]
				if !exists || rem.IsDir != d.IsDir() {
					c.logf("DELETE-LOCAL %s\n", childRel)
					if !c.cfg.DryRun {
						if err := localRoot.RemoveAll(childRel); err != nil {
							return fmt.Errorf("remove local %q: %w", childRel, err)
						}
					}
				}
			}
		}
	}

	for _, entry := range remoteEntries {
		childRel := entry.Name
		if relDir != "." {
			childRel = path.Join(relDir, entry.Name)
		}

		if entry.IsDir {
			if !c.cfg.DryRun {
				if err := localRoot.MkdirAll(childRel, 0755); err != nil {
					return fmt.Errorf("local mkdir %q: %w", childRel, err)
				}
			}
			if err := syncDownloadDir(c, localRoot, childRel); err != nil {
				return err
			}
			continue
		}

		// Regular file: check if local file exists with identical size (unless -all is set).
		if !c.cfg.All && localRoot != nil {
			if info, err := localRoot.Stat(childRel); err == nil && !info.IsDir() && info.Size() == entry.Size {
				continue
			}
		}

		if err := downloadFile(c, localRoot, childRel); err != nil {
			return err
		}
	}
	return nil
}

func downloadFile(c *Client, localRoot *os.Root, relPath string) error {
	if c.cfg.DryRun {
		c.logRequest(http.MethodGet, relPath, "")
		return nil
	}
	rc, err := c.Get(relPath)
	if err != nil {
		return err
	}
	defer rc.Close()

	f, err := localRoot.OpenFile(relPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("open local %q: %w", relPath, err)
	}
	_, copyErr := io.Copy(f, rc)
	closeErr := f.Close()
	if copyErr != nil {
		return fmt.Errorf("write local %q: %w", relPath, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close local %q: %w", relPath, closeErr)
	}
	return nil
}

// SyncUpload recursively uploads files from localRoot to the remote server
// according to c.cfg.
func SyncUpload(c *Client, localRoot *os.Root) error {
	return syncUploadDir(c, localRoot, ".")
}

func syncUploadDir(c *Client, localRoot *os.Root, relDir string) error {
	remoteMap := make(map[string]Entry)
	remoteEntries, err := c.LS(relDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, e := range remoteEntries {
		remoteMap[e.Name] = e
	}

	dirFile, err := localRoot.Open(relDir)
	if err != nil {
		return fmt.Errorf("open local dir %q: %w", relDir, err)
	}
	localEntries, err := dirFile.ReadDir(-1)
	_ = dirFile.Close()
	if err != nil {
		return fmt.Errorf("readdir local %q: %w", relDir, err)
	}
	slices.SortFunc(localEntries, func(a, b os.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})

	if c.cfg.Delete && len(remoteEntries) > 0 {
		localIsDir := make(map[string]bool, len(localEntries))
		for _, d := range localEntries {
			name := d.Name()
			childRel := name
			if relDir != "." {
				childRel = path.Join(relDir, name)
			}
			if info, err := localRoot.Stat(childRel); err == nil {
				localIsDir[name] = info.IsDir()
			}
		}
		for _, rem := range remoteEntries {
			childRel := rem.Name
			if relDir != "." {
				childRel = path.Join(relDir, rem.Name)
			}
			locDir, exists := localIsDir[rem.Name]
			if !exists || locDir != rem.IsDir {
				if err := c.RemoveAll(childRel, rem.IsDir); err != nil {
					return err
				}
				delete(remoteMap, rem.Name)
			}
		}
	}

	for _, d := range localEntries {
		name := d.Name()
		childRel := name
		if relDir != "." {
			childRel = path.Join(relDir, name)
		}

		info, err := localRoot.Stat(childRel)
		if err != nil {
			return fmt.Errorf("stat local %q: %w", childRel, err)
		}

		if info.IsDir() {
			if rem, ok := remoteMap[name]; !ok || !rem.IsDir {
				if err := c.MkdirAll(childRel); err != nil {
					return err
				}
			}
			if err := syncUploadDir(c, localRoot, childRel); err != nil {
				return err
			}
			continue
		}

		// Regular file: check if remote file already has the same size (unless -all is set).
		if !c.cfg.All {
			if rem, ok := remoteMap[name]; ok && !rem.IsDir && rem.Size == info.Size() {
				continue
			}
		}

		if err := uploadFile(c, localRoot, childRel); err != nil {
			return err
		}
	}
	return nil
}

func uploadFile(c *Client, localRoot *os.Root, relPath string) error {
	if c.cfg.DryRun {
		return c.Put(relPath, nil)
	}
	f, err := localRoot.Open(relPath)
	if err != nil {
		return fmt.Errorf("open local %q: %w", relPath, err)
	}
	defer f.Close()

	return c.Put(relPath, f)
}

// Run executes the highway sync operation specified by cfg.
func Run(cfg Config) error {
	if err := ValidateFSFlag(cfg.FS); err != nil {
		return err
	}
	client, err := NewClient(cfg)
	if err != nil {
		return err
	}

	switch cfg.Cmd {
	case "download":
		var localRoot *os.Root
		if cfg.DryRun {
			if info, err := os.Stat(cfg.FS); err == nil && info.IsDir() {
				localRoot, err = os.OpenRoot(cfg.FS)
				if err != nil {
					return fmt.Errorf("open root %q: %w", cfg.FS, err)
				}
				defer localRoot.Close()
			}
		} else {
			if err := os.MkdirAll(cfg.FS, 0755); err != nil {
				return fmt.Errorf("mkdir %q: %w", cfg.FS, err)
			}
			localRoot, err = os.OpenRoot(cfg.FS)
			if err != nil {
				return fmt.Errorf("open root %q: %w", cfg.FS, err)
			}
			defer localRoot.Close()
		}
		return SyncDownload(client, localRoot)

	case "upload":
		info, err := os.Stat(cfg.FS)
		if err != nil {
			return fmt.Errorf("stat %q: %w", cfg.FS, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%q is not a directory", cfg.FS)
		}
		localRoot, err := os.OpenRoot(cfg.FS)
		if err != nil {
			return fmt.Errorf("open root %q: %w", cfg.FS, err)
		}
		defer localRoot.Close()
		return SyncUpload(client, localRoot)

	default:
		return fmt.Errorf("invalid -c=%q: must be \"download\" or \"upload\"", cfg.Cmd)
	}
}

func main() {
	flag.Parse()

	cfg := Config{
		FS:      *flagFS,
		URL:     *flagURL,
		Cmd:     *flagCmd,
		All:     *flagAll,
		Delete:  *flagDelete,
		DryRun:  *flagDryRun,
		Verbose: *flagVerbose,
	}
	if err := Run(cfg); err != nil {
		log.Fatalf("Fatal: %v", err)
	}
}
