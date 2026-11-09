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
	"bufio"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
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
	"strconv"
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

// PeerState holds the generic in-memory state blob for an entity/client in a Room.
type PeerState struct {
	ID      string
	Blob    string
	Seq     uint32
	Updated time.Time
}

// ChatMessage holds a broadcast message entry in a Room's recent history ring buffer.
type ChatMessage struct {
	ID   string
	Text string
	Seq  uint32
	Time time.Time
}

type wsClient struct {
	id      string
	conn    net.Conn
	writeMu sync.Mutex
}

func (c *wsClient) sendText(msg string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	return wsWriteFrame(c.conn, 0x01, []byte(msg))
}

const maxRoomHistory = 40

// Room coordinates real-time RAM state blobs, chat/message history, and WebSocket broadcasts for an app channel.
type Room struct {
	mu      sync.Mutex
	name    string
	states  map[string]*PeerState
	clients map[*wsClient]struct{}
	history []ChatMessage
	nextSeq uint32
}

func newRoom(name string) *Room {
	return &Room{
		name:    name,
		states:  make(map[string]*PeerState),
		clients: make(map[*wsClient]struct{}),
	}
}

// WorldHub manages named multi-user rooms (e.g. "croquet", "chat", or any "/ws/<room>").
type WorldHub struct {
	mu    sync.Mutex
	rooms map[string]*Room
}

func newWorldHub() *WorldHub {
	return &WorldHub{
		rooms: make(map[string]*Room),
	}
}

func (h *WorldHub) getRoom(name string) *Room {
	clean := strings.ToLower(strings.TrimSpace(name))
	if clean == "" || clean == "." {
		clean = "default"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rm, ok := h.rooms[clean]
	if !ok {
		rm = newRoom(clean)
		h.rooms[clean] = rm
	}
	return rm
}

// Server serves a rooted filesystem over HTTP and hosts the in-memory Croquet WorldHub.
type Server struct {
	root        *os.Root
	password    string
	auditMu     sync.Mutex
	auditWriter io.Writer
	world       *WorldHub
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
		world:       newWorldHub(),
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
		user = r.URL.Query().Get("user")
		pass = r.URL.Query().Get("pass")
		ok = (user != "" || pass != "")
	}
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

	if rel == "croquet" || rel == "chat" || strings.HasPrefix(rel, "ws/") || strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		roomName := strings.TrimPrefix(rel, "ws/")
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			s.handleRoomWS(w, r, user, roomName)
			return
		}
		if strings.EqualFold(r.Method, http.MethodGet) {
			s.handleRoomSnapshot(w, roomName)
			return
		}
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
		fmt.Fprint(w, "1\n")
		fmt.Fprint(w, formatLSEntry(info.Name(), info))
		fmt.Fprint(w, ";\n")
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

	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		eInfo, err := entry.Info()
		if err != nil {
			continue
		}
		lines = append(lines, formatLSEntry(entry.Name(), eInfo))
	}

	fmt.Fprintf(w, "%d\n", len(lines))
	for _, line := range lines {
		fmt.Fprint(w, line)
	}
	fmt.Fprint(w, ";\n")
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

// ---------------------------------------------------------------------------
// Croquet Multi-User 3D World RAM Hub & RFC 6455 WebSocket Handler
// ---------------------------------------------------------------------------

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func wsAcceptKey(clientKey string) string {
	h := sha1.New()
	_, _ = h.Write([]byte(clientKey))
	_, _ = h.Write([]byte(wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func wsWriteFrame(w io.Writer, opcode byte, payload []byte) error {
	var hdr [10]byte
	hdr[0] = 0x80 | (opcode & 0x0F)
	n := len(payload)
	hdrLen := 2
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 65535:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		hdrLen = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		hdrLen = 10
	}
	if _, err := w.Write(hdr[:hdrLen]); err != nil {
		return err
	}
	if n > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func wsReadFrame(r io.Reader) (byte, []byte, error) {
	var b2 [2]byte
	if _, err := io.ReadFull(r, b2[:]); err != nil {
		return 0, nil, err
	}
	opcode := b2[0] & 0x0F
	masked := (b2[1] & 0x80) != 0
	length := uint64(b2[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > 4096 {
		return 0, nil, errors.New("websocket frame too large")
	}
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(r, maskKey[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, int(length))
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= maskKey[i&3]
			}
		}
	}
	return opcode, payload, nil
}

func sanitizeAvatarID(raw string) string {
	var b strings.Builder
	for i := 0; i < len(raw) && b.Len() < 24; i++ {
		c := raw[i]
		if c > 32 && c <= 126 {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func sanitizeSingleLineBlob(raw string, maxLen int) string {
	raw = strings.TrimSpace(raw)
	var b strings.Builder
	for i := 0; i < len(raw) && b.Len() < maxLen; i++ {
		c := raw[i]
		if c >= 32 && c <= 126 {
			b.WriteByte(c)
		} else if c == '\t' {
			b.WriteByte(' ')
		}
	}
	return strings.TrimSpace(b.String())
}

func formatPeerState(ps *PeerState) string {
	return "STATE " + ps.ID + " " + ps.Blob
}

func formatChatMessage(m ChatMessage) string {
	return "MSG " + m.ID + " " + m.Text
}

func (s *Server) handleRoomSnapshot(w http.ResponseWriter, roomName string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	room := s.world.getRoom(roomName)
	now := time.Now()
	room.mu.Lock()
	var stateLines []string
	for id, ps := range room.states {
		if now.Sub(ps.Updated) > 90*time.Second {
			active := false
			for c := range room.clients {
				if c.id == id {
					active = true
					break
				}
			}
			if !active {
				delete(room.states, id)
				continue
			}
		}
		stateLines = append(stateLines, formatPeerState(ps))
	}
	var msgLines []string
	for _, m := range room.history {
		msgLines = append(msgLines, formatChatMessage(m))
	}
	room.mu.Unlock()
	slices.Sort(stateLines)
	for _, l := range stateLines {
		fmt.Fprintln(w, l)
	}
	for _, l := range msgLines {
		fmt.Fprintln(w, l)
	}
}

func (s *Server) handleRoomWS(w http.ResponseWriter, r *http.Request, authUser, roomName string) {
	wsKey := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if wsKey == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
	}

	accept := wsAcceptKey(wsKey)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		return
	}

	avatarID := sanitizeAvatarID(r.URL.Query().Get("id"))
	if avatarID == "" {
		avatarID = sanitizeAvatarID(authUser)
	}
	if avatarID == "" {
		avatarID = "cyd"
	}

	client := &wsClient{
		id:   avatarID,
		conn: conn,
	}

	room := s.world.getRoom(roomName)

	// Register client and ensure unique avatarID if another active session shares the same base name
	room.mu.Lock()
	baseID := avatarID
	suffix := 2
	for {
		conflict := false
		for existing := range room.clients {
			if existing.id == avatarID {
				conflict = true
				break
			}
		}
		if !conflict {
			break
		}
		avatarID = fmt.Sprintf("%s-%d", baseID, suffix)
		suffix++
	}
	client.id = avatarID
	room.clients[client] = struct{}{}

	// Collect current snapshot of active peer states and recent messages in RAM
	now := time.Now()
	var snapshot []string
	for id, ps := range room.states {
		if now.Sub(ps.Updated) > 90*time.Second {
			active := false
			for c := range room.clients {
				if c.id == id {
					active = true
					break
				}
			}
			if !active {
				delete(room.states, id)
				continue
			}
		}
		snapshot = append(snapshot, formatPeerState(ps))
	}
	slices.Sort(snapshot)
	for _, m := range room.history {
		snapshot = append(snapshot, formatChatMessage(m))
	}
	room.mu.Unlock()

	// Send initial assignment, state snapshot, and message history to the newly connected client
	_ = client.sendText("YOU " + client.id)
	for _, msg := range snapshot {
		_ = client.sendText(msg)
	}

	defer func() {
		var peers []*wsClient
		room.mu.Lock()
		delete(room.clients, client)
		delete(room.states, client.id)
		for p := range room.clients {
			peers = append(peers, p)
		}
		room.mu.Unlock()

		leaveMsg := "LEAVE " + client.id
		for _, p := range peers {
			_ = p.sendText(leaveMsg)
		}
	}()

	var reader io.Reader = conn
	if brw != nil && brw.Reader != nil && brw.Reader.Buffered() > 0 {
		reader = io.MultiReader(io.LimitReader(brw.Reader, int64(brw.Reader.Buffered())), conn)
	}
	bufReader := bufio.NewReaderSize(reader, 1024)

	for {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		opcode, payload, err := wsReadFrame(bufReader)
		if err != nil {
			return
		}
		switch opcode {
		case 0x08: // Close
			_ = wsWriteFrame(conn, 0x08, nil)
			return
		case 0x09: // Ping -> Pong
			client.writeMu.Lock()
			_ = wsWriteFrame(conn, 0x0A, payload)
			client.writeMu.Unlock()
		case 0x01, 0x02: // Text or Binary command line(s)
			lines := strings.Split(string(payload), "\n")
			for _, rawLine := range lines {
				line := strings.TrimSpace(rawLine)
				if line == "" {
					continue
				}
				s.processRoomLine(room, client, line)
			}
		}
	}
}

func (s *Server) processRoomLine(room *Room, client *wsClient, line string) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return
	}
	cmd := strings.ToUpper(fields[0])
	rest := ""
	if len(line) > len(fields[0]) {
		rest = sanitizeSingleLineBlob(line[len(fields[0]):], 240)
	}

	switch cmd {
	case "PING":
		_ = client.sendText("PONG")
	case "ID":
		if len(fields) < 2 {
			return
		}
		newID := sanitizeAvatarID(fields[1])
		if newID == "" || newID == client.id {
			return
		}
		var peers []*wsClient
		var oldID string
		var stateMsg string

		room.mu.Lock()
		for existing := range room.clients {
			if existing != client && existing.id == newID {
				room.mu.Unlock()
				return
			}
		}
		oldID = client.id
		client.id = newID
		if ps, ok := room.states[oldID]; ok {
			delete(room.states, oldID)
			ps.ID = newID
			ps.Updated = time.Now()
			room.nextSeq++
			ps.Seq = room.nextSeq
			room.states[newID] = ps
			stateMsg = formatPeerState(ps)
		}
		for p := range room.clients {
			if p != client {
				peers = append(peers, p)
			}
		}
		room.mu.Unlock()

		_ = client.sendText("YOU " + newID)
		for _, p := range peers {
			_ = p.sendText("LEAVE " + oldID)
			if stateMsg != "" {
				_ = p.sendText(stateMsg)
			}
		}

	case "SET", "STATE":
		// Generic opaque per-peer state blob: SET <blob> -> broadcasts STATE <id> <blob>
		if rest == "" {
			return
		}
		var peers []*wsClient
		var deltaMsg string

		room.mu.Lock()
		room.nextSeq++
		seq := room.nextSeq
		ps, ok := room.states[client.id]
		if !ok {
			ps = &PeerState{ID: client.id}
			room.states[client.id] = ps
		}
		ps.Blob = rest
		ps.Seq = seq
		ps.Updated = time.Now()
		deltaMsg = formatPeerState(ps)
		for p := range room.clients {
			if p != client {
				peers = append(peers, p)
			}
		}
		room.mu.Unlock()

		for _, p := range peers {
			_ = p.sendText(deltaMsg)
		}

	case "POSE":
		// POSE <x> <y> <z> <theta> [color] (or generic POSE <blob>)
		if len(fields) < 5 {
			return
		}
		x, err1 := strconv.ParseFloat(fields[1], 64)
		y, err2 := strconv.ParseFloat(fields[2], 64)
		z, err3 := strconv.ParseFloat(fields[3], 64)
		theta, err4 := strconv.ParseFloat(fields[4], 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			return
		}
		col := 0
		if len(fields) >= 6 {
			if c, err := strconv.Atoi(fields[5]); err == nil {
				col = c
			}
		}

		var peers []*wsClient
		var deltaMsg string

		room.mu.Lock()
		room.nextSeq++
		seq := room.nextSeq
		ps, ok := room.states[client.id]
		if !ok {
			ps = &PeerState{ID: client.id}
			room.states[client.id] = ps
		}
		ps.Blob = fmt.Sprintf("%.2f %.2f %.2f %.3f %d %d", x, y, z, theta, col, seq)
		ps.Seq = seq
		ps.Updated = time.Now()
		deltaMsg = formatPeerState(ps)
		for p := range room.clients {
			if p != client {
				peers = append(peers, p)
			}
		}
		room.mu.Unlock()

		for _, p := range peers {
			_ = p.sendText(deltaMsg)
		}

	case "MSG", "SAY":
		// Multi-user chat / event broadcast with room scrollback history:
		// MSG <text> -> broadcasts MSG <id> <text> to all connected clients in the room
		if rest == "" {
			return
		}
		var allClients []*wsClient
		var chatLine string

		room.mu.Lock()
		room.nextSeq++
		msg := ChatMessage{
			ID:   client.id,
			Text: rest,
			Seq:  room.nextSeq,
			Time: time.Now(),
		}
		room.history = append(room.history, msg)
		if len(room.history) > maxRoomHistory {
			room.history = room.history[len(room.history)-maxRoomHistory:]
		}
		if ps, ok := room.states[client.id]; ok {
			ps.Updated = msg.Time
		}
		chatLine = formatChatMessage(msg)
		for p := range room.clients {
			allClients = append(allClients, p)
		}
		room.mu.Unlock()

		for _, p := range allClients {
			_ = p.sendText(chatLine)
		}

	case "BCAST", "PUB":
		// Ephemeral broadcast to all clients in the room without storing in history
		if rest == "" {
			return
		}
		var allClients []*wsClient
		room.mu.Lock()
		chatLine := "MSG " + client.id + " " + rest
		for p := range room.clients {
			allClients = append(allClients, p)
		}
		room.mu.Unlock()

		for _, p := range allClients {
			_ = p.sendText(chatLine)
		}
	}
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
