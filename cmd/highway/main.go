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
	flagVerbose = flag.Bool("v", false, "log every HTTP request URL sent to the server to stderr")
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
func ParseLS(body string) ([]Entry, error) {
	body = strings.TrimSuffix(body, "\n")
	if body == "" {
		return nil, nil
	}
	lines := strings.Split(body, "\n")
	entries := make([]Entry, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		e, err := ParseLSLine(line)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// Client communicates with a remote superhighway-server.
type Client struct {
	baseURL    *url.URL
	username   string
	password   string
	hasAuth    bool
	verbose    bool
	logWriter  io.Writer
	httpClient *http.Client
}

// NewClient parses rawURL (extracting any embedded user:password) and returns a Client.
func NewClient(rawURL string) (*Client, error) {
	if rawURL == "" {
		return nil, errors.New("-url is required")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url %q: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme %q (want http or https)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("missing host in URL %q", rawURL)
	}

	c := &Client{
		logWriter:  os.Stderr,
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

func (c *Client) do(method, relPath, rawQuery string, body io.Reader) (*http.Response, error) {
	target := c.endpointURL(relPath, rawQuery)
	if c.verbose {
		w := c.logWriter
		if w == nil {
			w = os.Stderr
		}
		fmt.Fprintf(w, "%s %s\n", method, target)
	}
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
// whenever the local file is missing, differs in byte size, or forceAll is true.
// When deleteExtra is true, local files/directories not present on the remote server are removed.
func SyncDownload(c *Client, localRoot *os.Root, forceAll, deleteExtra bool) error {
	return syncDownloadDir(c, localRoot, ".", forceAll, deleteExtra)
}

func syncDownloadDir(c *Client, localRoot *os.Root, relDir string, forceAll, deleteExtra bool) error {
	remoteEntries, err := c.LS(relDir)
	if err != nil {
		return err
	}

	if deleteExtra {
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
					if err := localRoot.RemoveAll(childRel); err != nil {
						return fmt.Errorf("remove local %q: %w", childRel, err)
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
			if err := localRoot.MkdirAll(childRel, 0755); err != nil {
				return fmt.Errorf("local mkdir %q: %w", childRel, err)
			}
			if err := syncDownloadDir(c, localRoot, childRel, forceAll, deleteExtra); err != nil {
				return err
			}
			continue
		}

		// Regular file: check if local file exists with identical size (unless -all is set).
		if !forceAll {
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
// whenever the remote file is missing, differs in byte size, or forceAll is true.
// When deleteExtra is true, remote files/directories not present locally are removed.
func SyncUpload(c *Client, localRoot *os.Root, forceAll, deleteExtra bool) error {
	return syncUploadDir(c, localRoot, ".", forceAll, deleteExtra)
}

func syncUploadDir(c *Client, localRoot *os.Root, relDir string, forceAll, deleteExtra bool) error {
	remoteMap := make(map[string]Entry)
	remoteEntries, err := c.LS(relDir)
	if errors.Is(err, fs.ErrNotExist) {
		if relDir != "." {
			if err := c.MkdirAll(relDir); err != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	} else {
		for _, e := range remoteEntries {
			remoteMap[e.Name] = e
		}
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

	if deleteExtra && len(remoteEntries) > 0 {
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
			if err := syncUploadDir(c, localRoot, childRel, forceAll, deleteExtra); err != nil {
				return err
			}
			continue
		}

		// Regular file: check if remote file already has the same size (unless -all is set).
		if !forceAll {
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
	f, err := localRoot.Open(relPath)
	if err != nil {
		return fmt.Errorf("open local %q: %w", relPath, err)
	}
	defer f.Close()

	return c.Put(relPath, f)
}

// Run executes the highway command ("download" or "upload") given fsPath, rawURL, cmd, forceAll, deleteExtra, and verbose.
func Run(fsPath, rawURL, cmd string, forceAll, deleteExtra, verbose bool) error {
	if err := ValidateFSFlag(fsPath); err != nil {
		return err
	}
	client, err := NewClient(rawURL)
	if err != nil {
		return err
	}
	client.verbose = verbose

	switch cmd {
	case "download":
		if err := os.MkdirAll(fsPath, 0755); err != nil {
			return fmt.Errorf("mkdir %q: %w", fsPath, err)
		}
		localRoot, err := os.OpenRoot(fsPath)
		if err != nil {
			return fmt.Errorf("open root %q: %w", fsPath, err)
		}
		defer localRoot.Close()
		return SyncDownload(client, localRoot, forceAll, deleteExtra)

	case "upload":
		info, err := os.Stat(fsPath)
		if err != nil {
			return fmt.Errorf("stat %q: %w", fsPath, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%q is not a directory", fsPath)
		}
		localRoot, err := os.OpenRoot(fsPath)
		if err != nil {
			return fmt.Errorf("open root %q: %w", fsPath, err)
		}
		defer localRoot.Close()
		return SyncUpload(client, localRoot, forceAll, deleteExtra)

	default:
		return fmt.Errorf("invalid -c=%q: must be \"download\" or \"upload\"", cmd)
	}
}

func main() {
	flag.Parse()

	if err := Run(*flagFS, *flagURL, *flagCmd, *flagAll, *flagDelete, *flagVerbose); err != nil {
		log.Fatalf("Fatal: %v", err)
	}
}
