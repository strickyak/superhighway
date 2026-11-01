package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseLSLine(t *testing.T) {
	line := "2026-10-31 21:05:09 12345 Filename might have spaces and #@%$%? funny chars!"
	e, err := ParseLSLine(line)
	if err != nil {
		t.Fatalf("ParseLSLine: %v", err)
	}
	if e.Name != "Filename might have spaces and #@%$%? funny chars!" || e.Size != 12345 || e.IsDir {
		t.Errorf("got %+v", e)
	}

	dirLine := "2026-10-31 21:05:09 4096 my dir/"
	d, err := ParseLSLine(dirLine)
	if err != nil {
		t.Fatalf("ParseLSLine dir: %v", err)
	}
	if d.Name != "my dir" || !d.IsDir {
		t.Errorf("got %+v", d)
	}

	// Test full ParseLS with count and ';' terminator
	fullBody := "2\n" + line + "\n" + dirLine + "\n;\n"
	entries, err := ParseLS(fullBody)
	if err != nil || len(entries) != 2 {
		t.Fatalf("ParseLS got (%d entries, %v), want 2 entries", len(entries), err)
	}
	emptyBody := "0\n;\n"
	emptyEntries, err := ParseLS(emptyBody)
	if err != nil || len(emptyEntries) != 0 {
		t.Fatalf("ParseLS(empty) got (%v, %v), want 0 entries", emptyEntries, err)
	}
	if _, err := ParseLS("2\n" + line + "\n;\n"); err == nil {
		t.Errorf("expected error for count mismatch, got nil")
	}
	if _, err := ParseLS("1\n" + line + "\n"); err == nil {
		t.Errorf("expected error for missing ';' terminator, got nil")
	}
}

// mockSuperhighwayServer implements the server protocol in-process for testing highway sync
// and tracks GET and PUT counts to verify unchanged files are skipped.
type mockSuperhighwayServer struct {
	root     *os.Root
	user     string
	pass     string
	getCount atomic.Int32
	putCount atomic.Int32
}

func (m *mockSuperhighwayServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u, p, ok := r.BasicAuth()
	if !ok || u != m.user || p != m.pass {
		http.Error(w, "418 I'm a teapot", http.StatusTeapot)
		return
	}
	rel := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if rel == "" {
		rel = "."
	}

	switch r.Method {
	case "LS":
		info, err := m.root.Stat(rel)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if !info.IsDir() {
			fmt.Fprintf(w, "1\n%s %d %s\n;\n", info.ModTime().UTC().Format(timeFormat), info.Size(), info.Name())
			return
		}
		f, err := m.root.Open(rel)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		entries, _ := f.ReadDir(-1)
		slices.SortFunc(entries, func(a, b os.DirEntry) int {
			return strings.Compare(a.Name(), b.Name())
		})
		var lines []string
		for _, entry := range entries {
			ei, err := entry.Info()
			if err != nil {
				continue
			}
			suffix := ""
			if ei.IsDir() {
				suffix = "/"
			}
			lines = append(lines, fmt.Sprintf("%s %d %s%s\n", ei.ModTime().UTC().Format(timeFormat), ei.Size(), entry.Name(), suffix))
		}
		fmt.Fprintf(w, "%d\n", len(lines))
		for _, l := range lines {
			fmt.Fprint(w, l)
		}
		fmt.Fprint(w, ";\n")

	case "MKDIR":
		if r.URL.Query().Get("p") == "1" {
			if err := m.root.MkdirAll(rel, 0755); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		} else {
			if err := m.root.Mkdir(rel, 0755); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		w.WriteHeader(http.StatusCreated)

	case http.MethodGet:
		m.getCount.Add(1)
		f, err := m.root.Open(rel)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		defer f.Close()
		_, _ = io.Copy(w, f)

	case http.MethodPut:
		m.putCount.Add(1)
		f, err := m.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, _ = io.Copy(f, r.Body)
		_ = f.Close()
		w.WriteHeader(http.StatusCreated)

	case "RM", http.MethodDelete:
		info, err := m.root.Lstat(rel)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if info.IsDir() {
			http.Error(w, "is a dir", http.StatusBadRequest)
			return
		}
		if err := m.root.Remove(rel); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)

	case "RMDIR":
		info, err := m.root.Lstat(rel)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if !info.IsDir() {
			http.Error(w, "not a dir", http.StatusBadRequest)
			return
		}
		if err := m.root.Remove(rel); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}

func TestUploadAndDownloadSync(t *testing.T) {
	tmp := t.TempDir()
	serverFS := filepath.Join(tmp, "server", "fs") + "/"
	srcFS := filepath.Join(tmp, "src", "fs") + "/"
	dstFS := filepath.Join(tmp, "dst", "fs") + "/"

	for _, dir := range []string{serverFS, srcFS} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
	}

	srvRoot, err := os.OpenRoot(serverFS)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer srvRoot.Close()

	mockSrv := &mockSuperhighwayServer{
		root: srvRoot,
		user: "strick",
		pass: "pw123",
	}
	ts := httptest.NewServer(mockSrv)
	defer ts.Close()

	parsedTS, _ := url.Parse(ts.URL)
	parsedTS.User = url.UserPassword("strick", "pw123")
	authURL := parsedTS.String() + "/"

	// Populate srcFS with files including nested directory and funny filename
	funnyName := "Filename with spaces & #@%$%? chars!"
	if err := os.WriteFile(filepath.Join(srcFS, "a.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcFS, "sub", "deep"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcFS, "sub", "deep", funnyName), []byte("0123456789"), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Initial upload: should PUT 2 files
	if err := Run(Config{FS: srcFS, URL: authURL, Cmd: "upload"}); err != nil {
		t.Fatalf("Run upload 1: %v", err)
	}
	if got := mockSrv.putCount.Load(); got != 2 {
		t.Fatalf("putCount after initial upload = %d, want 2", got)
	}

	// 2. Second upload with no size changes: should PUT 0 files
	mockSrv.putCount.Store(0)
	if err := Run(Config{FS: srcFS, URL: authURL, Cmd: "upload"}); err != nil {
		t.Fatalf("Run upload 2: %v", err)
	}
	if got := mockSrv.putCount.Load(); got != 0 {
		t.Fatalf("putCount after unchanged upload = %d, want 0", got)
	}

	// 2b. Upload with All=true (-all): should PUT all 2 files even with identical sizes
	if err := Run(Config{FS: srcFS, URL: authURL, Cmd: "upload", All: true}); err != nil {
		t.Fatalf("Run upload -all: %v", err)
	}
	if got := mockSrv.putCount.Load(); got != 2 {
		t.Fatalf("putCount after -all upload = %d, want 2", got)
	}
	mockSrv.putCount.Store(0)

	// 3. Modify a.txt size locally and re-upload: should PUT only 1 file (a.txt)
	if err := os.WriteFile(filepath.Join(srcFS, "a.txt"), []byte("hello world!"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Run(Config{FS: srcFS, URL: authURL, Cmd: "upload"}); err != nil {
		t.Fatalf("Run upload 3: %v", err)
	}
	if got := mockSrv.putCount.Load(); got != 1 {
		t.Fatalf("putCount after changing 1 file = %d, want 1", got)
	}

	// 4. Initial download into dstFS: should GET 2 files
	if err := Run(Config{FS: dstFS, URL: authURL, Cmd: "download"}); err != nil {
		t.Fatalf("Run download 1: %v", err)
	}
	if got := mockSrv.getCount.Load(); got != 2 {
		t.Fatalf("getCount after initial download = %d, want 2", got)
	}

	// Verify downloaded contents
	gotA, err := os.ReadFile(filepath.Join(dstFS, "a.txt"))
	if err != nil || string(gotA) != "hello world!" {
		t.Errorf("dst a.txt = %q (%v), want \"hello world!\"", string(gotA), err)
	}
	gotFunny, err := os.ReadFile(filepath.Join(dstFS, "sub", "deep", funnyName))
	if err != nil || string(gotFunny) != "0123456789" {
		t.Errorf("dst funny = %q (%v), want \"0123456789\"", string(gotFunny), err)
	}

	// 5. Second download with no size changes: should GET 0 files
	mockSrv.getCount.Store(0)
	if err := Run(Config{FS: dstFS, URL: authURL, Cmd: "download"}); err != nil {
		t.Fatalf("Run download 2: %v", err)
	}
	if got := mockSrv.getCount.Load(); got != 0 {
		t.Fatalf("getCount after unchanged download = %d, want 0", got)
	}

	// 5b. Download with All=true (-all): should GET all 2 files even with identical sizes
	if err := Run(Config{FS: dstFS, URL: authURL, Cmd: "download", All: true}); err != nil {
		t.Fatalf("Run download -all: %v", err)
	}
	if got := mockSrv.getCount.Load(); got != 2 {
		t.Fatalf("getCount after -all download = %d, want 2", got)
	}
	mockSrv.getCount.Store(0)

	// 6. Change size of funny file on server and download again: should GET only 1 file
	if err := os.WriteFile(filepath.Join(serverFS, "sub", "deep", funnyName), []byte("short"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Run(Config{FS: dstFS, URL: authURL, Cmd: "download"}); err != nil {
		t.Fatalf("Run download 3: %v", err)
	}
	if got := mockSrv.getCount.Load(); got != 1 {
		t.Fatalf("getCount after changing 1 remote file = %d, want 1", got)
	}
	gotFunny2, err := os.ReadFile(filepath.Join(dstFS, "sub", "deep", funnyName))
	if err != nil || string(gotFunny2) != "short" {
		t.Errorf("dst funny after update = %q (%v), want \"short\"", string(gotFunny2), err)
	}

	// 7. Test -delete on download: create extra file and dir in dstFS, verify -delete removes them
	extraLocalDir := filepath.Join(dstFS, "obsolete_dir", "nested")
	if err := os.MkdirAll(extraLocalDir, 0755); err != nil {
		t.Fatal(err)
	}
	extraLocalFile := filepath.Join(extraLocalDir, "old.txt")
	if err := os.WriteFile(extraLocalFile, []byte("delete me"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Run(Config{FS: dstFS, URL: authURL, Cmd: "download", Delete: true}); err != nil {
		t.Fatalf("Run download -delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstFS, "obsolete_dir")); !os.IsNotExist(err) {
		t.Errorf("expected local obsolete_dir to be deleted, stat err = %v", err)
	}

	// 8. Test -delete on upload: create extra file and dir on serverFS, verify -delete removes them
	extraRemoteDir := filepath.Join(serverFS, "stale_remote", "inner")
	if err := os.MkdirAll(extraRemoteDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extraRemoteDir, "gone.txt"), []byte("bye"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Run(Config{FS: srcFS, URL: authURL, Cmd: "upload", Delete: true}); err != nil {
		t.Fatalf("Run upload -delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(serverFS, "stale_remote")); !os.IsNotExist(err) {
		t.Errorf("expected remote stale_remote to be deleted, stat err = %v", err)
	}

	// 9. Test -v verbose logging logs every HTTP request URL sent to the server
	var verboseBuf bytes.Buffer
	if err := Run(Config{
		FS:        dstFS,
		URL:       authURL,
		Cmd:       "download",
		All:       true,
		Verbose:   true,
		LogWriter: &verboseBuf,
	}); err != nil {
		t.Fatalf("Run download verbose: %v", err)
	}
	vOut := verboseBuf.String()
	for _, wantSub := range []string{"LS " + ts.URL + "/", "GET " + ts.URL + "/a.txt"} {
		if !strings.Contains(vOut, wantSub) {
			t.Errorf("verbose log missing %q; got:\n%s", wantSub, vOut)
		}
	}

	// 10. Test -n -v (dry-run + verbose): prints planned PUT/GET/RM/RMDIR/DELETE-LOCAL without modifying anything
	mockSrv.putCount.Store(0)
	mockSrv.getCount.Store(0)
	if err := os.WriteFile(filepath.Join(serverFS, "only_remote.txt"), []byte("keep me"), 0644); err != nil {
		t.Fatal(err)
	}

	verboseBuf.Reset()
	if err := Run(Config{
		FS:        srcFS,
		URL:       authURL,
		Cmd:       "upload",
		All:       true,
		Delete:    true,
		DryRun:    true,
		Verbose:   true,
		LogWriter: &verboseBuf,
	}); err != nil {
		t.Fatalf("Run upload dry-run: %v", err)
	}
	if got := mockSrv.putCount.Load(); got != 0 {
		t.Errorf("dry-run upload sent %d PUT requests, want 0", got)
	}
	if _, err := os.Stat(filepath.Join(serverFS, "only_remote.txt")); err != nil {
		t.Errorf("dry-run upload -delete actually deleted only_remote.txt: %v", err)
	}
	dryUploadLog := verboseBuf.String()
	for _, wantSub := range []string{"RM " + ts.URL + "/only_remote.txt", "PUT " + ts.URL + "/a.txt"} {
		if !strings.Contains(dryUploadLog, wantSub) {
			t.Errorf("dry-run upload log missing %q; got:\n%s", wantSub, dryUploadLog)
		}
	}

	// 11. Reject -fs not ending with /fs/
	if err := Run(Config{FS: "/tmp/bad", URL: authURL, Cmd: "download"}); err == nil {
		t.Errorf("expected error for invalid -fs, got nil")
	}
	_ = fs.ErrNotExist
}
