package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testUser = "alice"
	testPass = "PW123"
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

func TestSanitizeAndValidateUsername(t *testing.T) {
	if got := SanitizeUsername("ca\x07f\n\x7fé"); got != "caf" {
		t.Errorf("SanitizeUsername = %q, want \"caf\"", got)
	}

	valid := []string{
		"a",
		"alice",
		"bob_123-xyz",
		"ca\x07fé", // sanitizes to "caf"
		strings.Repeat("x", 32),
	}
	for _, u := range valid {
		if !ValidateUsername(u) {
			t.Errorf("ValidateUsername(%q) = false, want true", u)
		}
	}

	invalid := []string{
		"",
		"\x00\x07\x7f", // empty after stripping outside 32..126
		strings.Repeat("x", 33),
	}
	for _, u := range invalid {
		if ValidateUsername(u) {
			t.Errorf("ValidateUsername(%q) = true, want false", u)
		}
	}
}

func newTestServer(t *testing.T) (*httptest.Server, string, *bytes.Buffer) {
	t.Helper()
	tmp := t.TempDir()
	fsDir := filepath.Join(tmp, "fs") + "/"
	if err := os.MkdirAll(fsDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	srv, err := NewServer(fsDir, testPass)
	if err != nil {
		t.Fatalf("NewServer(%q): %v", fsDir, err)
	}
	var auditBuf bytes.Buffer
	srv.auditWriter = &auditBuf

	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, fsDir, &auditBuf
}

func doReqAuth(t *testing.T, method, targetURL, user, pass, body string) (int, string, http.Header) {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, targetURL, bodyReader)
	if err != nil {
		t.Fatalf("NewRequest(%s %s): %v", method, targetURL, err)
	}
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
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

func doReq(t *testing.T, method, targetURL, body string) (int, string, http.Header) {
	t.Helper()
	return doReqAuth(t, method, targetURL, testUser, testPass, body)
}

func TestAuthAndAuditLog(t *testing.T) {
	ts, _, auditBuf := newTestServer(t)

	// 1. Missing Basic Auth -> 418 I'm a teapot, no audit log
	code, _, _ := doReqAuth(t, "LS", ts.URL+"/", "", "", "")
	if code != http.StatusTeapot {
		t.Fatalf("no auth: got %d, want %d (418)", code, http.StatusTeapot)
	}

	// 2. Wrong password -> 418 I'm a teapot, no audit log
	code, _, _ = doReqAuth(t, http.MethodPut, ts.URL+"/x.txt", "alice", "wrong", "hello")
	if code != http.StatusTeapot {
		t.Fatalf("wrong pass: got %d, want %d (418)", code, http.StatusTeapot)
	}

	// 3. Username > 32 chars -> 418 I'm a teapot, no audit log
	code, _, _ = doReqAuth(t, "LS", ts.URL+"/", strings.Repeat("a", 33), testPass, "")
	if code != http.StatusTeapot {
		t.Fatalf("long user: got %d, want %d (418)", code, http.StatusTeapot)
	}

	// 4. Username with only non-printable chars -> 418 I'm a teapot, no audit log
	code, _, _ = doReqAuth(t, "LS", ts.URL+"/", "\x00\x07\x7f", testPass, "")
	if code != http.StatusTeapot {
		t.Fatalf("empty-after-sanitize user: got %d, want %d (418)", code, http.StatusTeapot)
	}

	if auditBuf.Len() != 0 {
		t.Fatalf("expected empty audit log after auth failures, got:\n%s", auditBuf.String())
	}

	// 5. Non-changing reads (LS, DU) -> 200 OK, no audit log
	code, _, _ = doReq(t, "LS", ts.URL+"/", "")
	if code != http.StatusOK {
		t.Fatalf("LS /: got %d, want 200", code)
	}
	code, _, _ = doReq(t, "DU", ts.URL+"/", "")
	if code != http.StatusOK {
		t.Fatalf("DU /: got %d, want 200", code)
	}
	if auditBuf.Len() != 0 {
		t.Fatalf("expected empty audit log after non-changing reads, got:\n%s", auditBuf.String())
	}

	// 6. Mutating requests (MKDIR, PUT, RM, RMDIR) -> logged as CSV starting with "csv,"
	code, _, _ = doReqAuth(t, "MKDIR", ts.URL+"/sub", "b\x07obé", testPass, "")
	if code != http.StatusCreated {
		t.Fatalf("MKDIR /sub: got %d, want 201", code)
	}
	code, _, _ = doReqAuth(t, http.MethodPut, ts.URL+"/sub/f.txt", "carol", testPass, "data")
	if code != http.StatusCreated {
		t.Fatalf("PUT /sub/f.txt: got %d, want 201", code)
	}
	// GET (read) should not add a log line
	code, _, _ = doReqAuth(t, http.MethodGet, ts.URL+"/sub/f.txt", "carol", testPass, "")
	if code != http.StatusOK {
		t.Fatalf("GET /sub/f.txt: got %d, want 200", code)
	}
	code, _, _ = doReqAuth(t, "RM", ts.URL+"/sub/f.txt", "dave", testPass, "")
	if code != http.StatusOK {
		t.Fatalf("RM /sub/f.txt: got %d, want 200", code)
	}
	code, _, _ = doReqAuth(t, "RMDIR", ts.URL+"/sub", "erin", testPass, "")
	if code != http.StatusOK {
		t.Fatalf("RMDIR /sub: got %d, want 200", code)
	}

	logLines := strings.Split(strings.TrimSuffix(auditBuf.String(), "\n"), "\n")
	if len(logLines) != 4 {
		t.Fatalf("expected 4 audit log lines, got %d:\n%s", len(logLines), auditBuf.String())
	}
	wantSuffixes := []string{
		`,127.0.0.1,bob,MKDIR,/sub`,
		`,127.0.0.1,carol,PUT,/sub/f.txt`,
		`,127.0.0.1,dave,RM,/sub/f.txt`,
		`,127.0.0.1,erin,RMDIR,/sub`,
	}
	for i, wantSuffix := range wantSuffixes {
		if !strings.HasPrefix(logLines[i], "csv,") || !strings.HasSuffix(logLines[i], wantSuffix) {
			t.Errorf("logLines[%d] = %q, want prefix \"csv,\" and suffix %q", i, logLines[i], wantSuffix)
		}
	}
}

func TestFilesystemLifecycle(t *testing.T) {
	ts, fsDir, _ := newTestServer(t)

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
	ts, fsDir, _ := newTestServer(t)

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

func TestTimeMethod(t *testing.T) {
	ts, _, auditBuf := newTestServer(t)

	before := time.Now().UTC().Unix()
	code, body, _ := doReq(t, "TIME", ts.URL+"/", "")
	after := time.Now().UTC().Unix()

	if code != http.StatusOK {
		t.Fatalf("TIME /: got %d (%s), want 200", code, body)
	}
	if auditBuf.Len() != 0 {
		t.Fatalf("TIME should not write to audit log, got: %s", auditBuf.String())
	}

	line := strings.TrimSuffix(body, "\n")
	fields := strings.Split(line, " ")
	if len(fields) != 3 {
		t.Fatalf("TIME response = %q, expected 3 space-separated fields (YYYY-mm-dd HH:MM:SS unix_sec)", line)
	}
	friendly := fields[0] + " " + fields[1]
	parsedTime, err := time.Parse(timeFormat, friendly)
	if err != nil {
		t.Fatalf("Parse(%q): %v", friendly, err)
	}
	unixSec, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		t.Fatalf("ParseInt(%q): %v", fields[2], err)
	}
	if parsedTime.Unix() != unixSec {
		t.Errorf("friendly time unix (%d) != unix_sec field (%d)", parsedTime.Unix(), unixSec)
	}
	if unixSec < before-2 || unixSec > after+2 {
		t.Errorf("unixSec = %d outside expected window [%d, %d]", unixSec, before-2, after+2)
	}
}
