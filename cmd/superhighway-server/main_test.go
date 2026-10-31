package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestValidateFSFlag(t *testing.T) {
	valid := []string{
		"./fs/",
		"dirname/fs/",
		"/tmp/my/fs/",
		"a/b/c/fs/",
	}
	for _, v := range valid {
		if err := ValidateFSFlag(v); err != nil {
			t.Errorf("ValidateFSFlag(%q) unexpected error: %v", v, err)
		}
	}

	invalid := []string{
		"",
		".",
		"/tmp",
		"fs",
		"fs/",
		"./fs",
		"dirname/fs",
		"/tmp/fs",
		"/fs",
		"dirname/fs/extra",
	}
	for _, inv := range invalid {
		if err := ValidateFSFlag(inv); err == nil {
			t.Errorf("ValidateFSFlag(%q) expected error, got nil", inv)
		}
	}
}

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	tmp := t.TempDir()
	fsDir := filepath.Join(tmp, "fs") + "/"
	if err := os.MkdirAll(fsDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	srv, err := NewServer(fsDir)
	if err != nil {
		t.Fatalf("NewServer(%q): %v", fsDir, err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, fsDir
}

func doReq(t *testing.T, method, targetURL, body string) (int, string, http.Header) {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, targetURL, bodyReader)
	if err != nil {
		t.Fatalf("NewRequest(%s %s): %v", method, targetURL, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do(%s %s): %v", method, targetURL, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return resp.StatusCode, string(b), resp.Header
}

func TestFilesystemLifecycle(t *testing.T) {
	ts, fsDir := newTestServer(t)

	// 1. MKDIR /docs
	code, body, _ := doReq(t, "MKDIR", ts.URL+"/docs", "")
	if code != http.StatusCreated {
		t.Fatalf("MKDIR /docs: got %d (%s), want 201", code, body)
	}

	// 2. PUT a normal file inside /docs
	content1 := "Hello, Superhighway!\n"
	code, body, _ = doReq(t, http.MethodPut, ts.URL+"/docs/hello.txt", content1)
	if code != http.StatusCreated {
		t.Fatalf("PUT /docs/hello.txt: got %d (%s), want 201", code, body)
	}

	// 3. PUT a file with spaces and funny characters in root
	funnyName := "Filename might have spaces and #@%$%? funny chars!"
	escapedName := (&url.URL{Path: "/" + funnyName}).EscapedPath()
	content2 := "0123456789abcdef" // 16 bytes
	code, body, _ = doReq(t, http.MethodPut, ts.URL+escapedName, content2)
	if code != http.StatusCreated {
		t.Fatalf("PUT %q: got %d (%s), want 201", funnyName, code, body)
	}

	// Set a known UTC timestamp on the funny file to test exact LS formatting
	fixedTime := time.Date(2026, 10, 31, 21, 5, 9, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(fsDir, funnyName), fixedTime, fixedTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// 4. GET the funny file back
	code, body, _ = doReq(t, http.MethodGet, ts.URL+escapedName, "")
	if code != http.StatusOK || body != content2 {
		t.Fatalf("GET %q: got %d %q, want 200 %q", funnyName, code, body, content2)
	}

	// 5. LS root "/"
	code, body, _ = doReq(t, "LS", ts.URL+"/", "")
	if code != http.StatusOK {
		t.Fatalf("LS /: got %d (%s), want 200", code, body)
	}
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("LS / expected 2 lines, got %d:\n%s", len(lines), body)
	}

	wantFunnyLine := "2026-10-31 21:05:09 16 " + funnyName
	if lines[0] != wantFunnyLine {
		t.Errorf("LS line[0] = %q, want %q", lines[0], wantFunnyLine)
	}

	dirRe := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} \d+ docs/$`)
	if !dirRe.MatchString(lines[1]) {
		t.Errorf("LS line[1] = %q, did not match %v", lines[1], dirRe)
	}

	// 6. DU on "/" and "/docs"
	code, body, _ = doReq(t, "DU", ts.URL+"/", "")
	if code != http.StatusOK || strings.TrimSpace(body) != "37" { // 21 + 16 = 37
		t.Errorf("DU /: got %d %q, want 200 \"37\"", code, strings.TrimSpace(body))
	}
	code, body, _ = doReq(t, "DU", ts.URL+"/docs", "")
	if code != http.StatusOK || strings.TrimSpace(body) != "21" {
		t.Errorf("DU /docs: got %d %q, want 200 \"21\"", code, strings.TrimSpace(body))
	}

	// 7. RMDIR on non-empty /docs must fail
	code, _, _ = doReq(t, "RMDIR", ts.URL+"/docs", "")
	if code == http.StatusOK {
		t.Fatalf("RMDIR non-empty /docs should fail, got 200")
	}

	// 8. RM on directory /docs must fail
	code, _, _ = doReq(t, "RM", ts.URL+"/docs", "")
	if code != http.StatusBadRequest {
		t.Fatalf("RM /docs directory: got %d, want 400", code)
	}

	// 9. RMDIR on file /docs/hello.txt must fail
	code, _, _ = doReq(t, "RMDIR", ts.URL+"/docs/hello.txt", "")
	if code != http.StatusBadRequest {
		t.Fatalf("RMDIR /docs/hello.txt file: got %d, want 400", code)
	}

	// 10. RM /docs/hello.txt
	code, body, _ = doReq(t, "RM", ts.URL+"/docs/hello.txt", "")
	if code != http.StatusOK {
		t.Fatalf("RM /docs/hello.txt: got %d (%s), want 200", code, body)
	}

	// 11. RMDIR /docs (now empty) must succeed
	code, body, _ = doReq(t, "RMDIR", ts.URL+"/docs", "")
	if code != http.StatusOK {
		t.Fatalf("RMDIR /docs: got %d (%s), want 200", code, body)
	}
}

func TestSymlinkEscapeBlocked(t *testing.T) {
	ts, fsDir := newTestServer(t)

	// Create a secret file outside fsDir
	outsideFile := filepath.Join(filepath.Dir(strings.TrimSuffix(fsDir, "/")), "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("top-secret"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Create a symlink inside fsDir pointing to ../secret.txt
	if err := os.Symlink("../secret.txt", filepath.Join(fsDir, "escape.txt")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// Attempting to GET /escape.txt must fail due to os.Root confinement
	code, _, _ := doReq(t, http.MethodGet, ts.URL+"/escape.txt", "")
	if code == http.StatusOK {
		t.Fatalf("GET /escape.txt succeeded across symlink boundary, expected error")
	}
}
