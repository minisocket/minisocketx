package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed web
var webFS embed.FS

var (
	listenAddr  string
	listenAddr6 string
	domain      string
	trustProxy  bool
	dataDir     string
)

// --- Input Validation & Sanitization ---

var validHexID = regexp.MustCompile(`^[0-9a-f]{1,64}$`)

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 32 && r != 127 {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 32 {
		out = out[:32]
	}
	if out == "" {
		out = "anonymous"
	}
	return out
}

// --- Rate Limiter ---

type rateLimiter struct {
	mu      sync.Mutex
	clients map[string]*rlBucket
}

type rlBucket struct {
	tokens float64
	last   time.Time
}

var sessionLimiter = newRateLimiter()
var generalLimiter = newRateLimiter()

// --- Per-IP WebSocket Connection Limiter ---

type wsConnTracker struct {
	mu    sync.Mutex
	conns map[string]int
}

var wsTracker = &wsConnTracker{conns: make(map[string]int)}

const maxWSPerIP = 20

func (t *wsConnTracker) acquire(ip string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conns[ip] >= maxWSPerIP {
		return false
	}
	t.conns[ip]++
	return true
}

func (t *wsConnTracker) release(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.conns[ip]--
	if t.conns[ip] <= 0 {
		delete(t.conns, ip)
	}
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func newRateLimiter() *rateLimiter {
	rl := &rateLimiter{clients: make(map[string]*rlBucket)}
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			rl.mu.Lock()
			cutoff := time.Now().Add(-10 * time.Minute)
			for ip, b := range rl.clients {
				if b.last.Before(cutoff) {
					delete(rl.clients, ip)
				}
			}
			rl.mu.Unlock()
		}
	}()
	return rl
}

func (rl *rateLimiter) allow(ip string, rate float64, burst int) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.clients[ip]
	if !ok {
		rl.clients[ip] = &rlBucket{tokens: float64(burst) - 1, last: time.Now()}
		return true
	}
	elapsed := time.Since(b.last).Seconds()
	b.tokens += elapsed * rate
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.last = time.Now()
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// --- Client IP (proxy-aware) ---

func clientIP(r *http.Request) string {
	if trustProxy {
		if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
			return stripIPv6Brackets(ip)
		}
		if ip := r.Header.Get("X-Real-IP"); ip != "" {
			return stripIPv6Brackets(ip)
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.Index(xff, ","); i > 0 {
				return stripIPv6Brackets(strings.TrimSpace(xff[:i]))
			}
			return stripIPv6Brackets(strings.TrimSpace(xff))
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func stripIPv6Brackets(ip string) string {
	return strings.TrimSuffix(strings.TrimPrefix(ip, "["), "]")
}

// --- WebSocket ---

func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	return origin == "https://"+domain || origin == "http://"+domain
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:    65536,
	WriteBufferSize:   65536,
	CheckOrigin:       checkOrigin,
	EnableCompression: false,
	HandshakeTimeout:  10 * time.Second,
}

type Client struct {
	conn     *websocket.Conn
	mu       sync.Mutex
	id       string
	closed   int32
	lastPong int64
}

func (c *Client) send(msg []byte) error {
	if atomic.LoadInt32(&c.closed) == 1 {
		return fmt.Errorf("closed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteMessage(websocket.TextMessage, msg)
}

func (c *Client) sendPing() error {
	if atomic.LoadInt32(&c.closed) == 1 {
		return fmt.Errorf("closed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteMessage(websocket.PingMessage, nil)
}

func (c *Client) markClosed() {
	atomic.StoreInt32(&c.closed, 1)
}

func (c *Client) isClosed() bool {
	return atomic.LoadInt32(&c.closed) == 1
}

func (c *Client) touchPong() {
	atomic.StoreInt64(&c.lastPong, time.Now().UnixNano())
}

func (c *Client) pongAge() time.Duration {
	last := atomic.LoadInt64(&c.lastPong)
	if last == 0 {
		return 0
	}
	return time.Since(time.Unix(0, last))
}

type Terminal struct {
	ID         string
	Name       string
	host       *Client
	mu         sync.RWMutex
	cols       int
	rows       int
	created    time.Time
	X          float64
	Y          float64
	outBuf     [][]byte
	graceTimer *time.Timer
}

const termBufMax = 64

func (t *Terminal) bufferOutput(msg []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cp := make([]byte, len(msg))
	copy(cp, msg)
	if len(t.outBuf) >= termBufMax {
		t.outBuf = t.outBuf[1:]
	}
	t.outBuf = append(t.outBuf, cp)
}

func (t *Terminal) replayTo(c *Client) {
	t.mu.RLock()
	buf := make([][]byte, len(t.outBuf))
	copy(buf, t.outBuf)
	t.mu.RUnlock()
	for _, msg := range buf {
		if c.isClosed() {
			return
		}
		c.send(msg)
	}
}

func (t *Terminal) sendToHost(msg []byte) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.host != nil {
		t.host.send(msg)
	}
}

type UserInfo struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Color    string  `json:"color"`
	Focus    string  `json:"focus"`
	CursorX  float64 `json:"cx,omitempty"`
	CursorY  float64 `json:"cy,omitempty"`
	Latency  int     `json:"latency,omitempty"`
	JoinedAt int64   `json:"joinedAt"`
}

type Session struct {
	ID           string
	hostSecret   string
	terminals    map[string]*Terminal
	viewers      map[*Client]*UserInfo
	mu           sync.RWMutex
	created      time.Time
	hostConn     *Client
	lastActivity int64
	deleteTimer  *time.Timer
}

func (s *Session) touch() {
	atomic.StoreInt64(&s.lastActivity, time.Now().UnixNano())
}

func (s *Session) idleDuration() time.Duration {
	last := atomic.LoadInt64(&s.lastActivity)
	if last == 0 {
		return time.Since(s.created)
	}
	return time.Since(time.Unix(0, last))
}

func (s *Session) addTerminal(t *Terminal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteTimer != nil {
		s.deleteTimer.Stop()
		s.deleteTimer = nil
		log.Printf("[session] reconnected, grace period cancelled")
	}
	s.terminals[t.ID] = t
}

func (s *Session) getTerminal(id string) *Terminal {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.terminals[id]
}

func (s *Session) removeTerminal(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.terminals, id)
}

func (s *Session) shellList() []map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]map[string]interface{}, 0, len(s.terminals))
	for _, t := range s.terminals {
		t.mu.RLock()
		active := t.host != nil
		t.mu.RUnlock()
		list = append(list, map[string]interface{}{
			"id": t.ID, "name": t.Name,
			"x": t.X, "y": t.Y,
			"cols": t.cols, "rows": t.rows,
			"active": active,
		})
	}
	return list
}

func (s *Session) addViewer(c *Client, u *UserInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.viewers[c] = u
}

func (s *Session) removeViewer(c *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.viewers, c)
}

func (s *Session) userList() []*UserInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*UserInfo, 0, len(s.viewers))
	for _, u := range s.viewers {
		list = append(list, u)
	}
	return list
}

func (s *Session) broadcastToViewers(msg []byte) {
	s.mu.RLock()
	clients := make([]*Client, 0, len(s.viewers))
	for c := range s.viewers {
		clients = append(clients, c)
	}
	s.mu.RUnlock()
	for _, c := range clients {
		c.send(msg)
	}
}

func (s *Session) broadcastToViewersExcept(msg []byte, except *Client) {
	s.mu.RLock()
	clients := make([]*Client, 0, len(s.viewers))
	for c := range s.viewers {
		if c != except {
			clients = append(clients, c)
		}
	}
	s.mu.RUnlock()
	for _, c := range clients {
		c.send(msg)
	}
}

func (s *Session) broadcastTermData(termID string, msg []byte) {
	n := len(msg)
	if n < 2 || msg[0] != '{' {
		return
	}
	tidField := `"tid":"` + termID + `",`
	tagged := make([]byte, 0, n+len(tidField))
	tagged = append(tagged, '{')
	tagged = append(tagged, tidField...)
	tagged = append(tagged, msg[1:]...)
	s.broadcastToViewers(tagged)
	if t := s.getTerminal(termID); t != nil {
		var check struct{ Type string `json:"t"` }
		if json.Unmarshal(msg, &check) == nil && check.Type == "data" {
			t.bufferOutput(tagged)
		}
	}
}

func (s *Session) broadcastShellList() {
	shells := s.shellList()
	data, _ := json.Marshal(map[string]interface{}{
		"t": "shells", "shells": shells,
	})
	s.broadcastToViewers(data)
	s.mu.RLock()
	if s.hostConn != nil {
		s.hostConn.send(data)
	}
	s.mu.RUnlock()
}

func (s *Session) broadcastUserUpdate() {
	users := s.userList()
	data, _ := json.Marshal(map[string]interface{}{
		"t": "users", "users": users,
	})
	s.broadcastToViewers(data)
}

func (s *Session) nextPosition() (float64, float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	type pos struct{ x, y float64 }
	existing := make([]pos, 0, len(s.terminals))
	for _, t := range s.terminals {
		existing = append(existing, pos{t.X, t.Y})
	}

	const tw, th, pad = 752, 515, 20
	for i := 0; i < 2000; i++ {
		angle := 1.94161 * float64(i)
		r := 8.0 * angle
		x := r * math.Cos(angle)
		y := r * math.Sin(angle)

		overlap := false
		for _, e := range existing {
			if math.Abs(x-e.x) < tw+pad && math.Abs(y-e.y) < th+pad {
				overlap = true
				break
			}
		}
		if !overlap {
			return x, y
		}
	}
	return 0, 0
}

func (s *Session) closeAllLocked() {
	for _, t := range s.terminals {
		t.mu.Lock()
		if t.host != nil {
			t.host.conn.Close()
		}
		t.mu.Unlock()
	}
	for c := range s.viewers {
		c.conn.Close()
	}
	if s.hostConn != nil {
		s.hostConn.conn.Close()
	}
}

func (s *Session) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeAllLocked()
}

var (
	sessions    = make(map[string]*Session)
	sessionsMu  sync.RWMutex
	startTime   = time.Now()
	healthToken = generateSecret()
)

var userColors = []string{
	"#58a6ff", "#3fb950", "#d29922", "#f85149",
	"#bc8cff", "#39c5cf", "#f0883e", "#db61a2",
	"#79c0ff", "#56d364", "#e3b341", "#ffa198",
}

func generateID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func shortID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)[:6]
}

func generateSecret() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// --- Session Persistence ---

type savedTerminal struct {
	ID   string  `json:"id"`
	Name string  `json:"name"`
	Cols int     `json:"cols"`
	Rows int     `json:"rows"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
}

type savedSession struct {
	ID         string          `json:"id"`
	HostSecret string          `json:"hostSecret"`
	Terminals  []savedTerminal `json:"terminals"`
	Created    time.Time       `json:"created"`
}

type savedState struct {
	HealthToken string         `json:"healthToken"`
	Sessions    []savedSession `json:"sessions"`
	SavedAt     time.Time      `json:"savedAt"`
}

func stateFilePath() string {
	return filepath.Join(dataDir, "state.json")
}

func saveSessions() {
	if dataDir == "" {
		return
	}
	sessionsMu.RLock()
	state := savedState{
		HealthToken: healthToken,
		SavedAt:     time.Now(),
	}
	for _, s := range sessions {
		s.mu.RLock()
		ss := savedSession{
			ID:         s.ID,
			HostSecret: s.hostSecret,
			Created:    s.created,
		}
		for _, t := range s.terminals {
			ss.Terminals = append(ss.Terminals, savedTerminal{
				ID: t.ID, Name: t.Name,
				Cols: t.cols, Rows: t.rows,
				X: t.X, Y: t.Y,
			})
		}
		s.mu.RUnlock()
		state.Sessions = append(state.Sessions, ss)
	}
	sessionsMu.RUnlock()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		log.Printf("[persist] marshal error: %v", err)
		return
	}
	tmp := stateFilePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		log.Printf("[persist] write error: %v", err)
		return
	}
	if err := os.Rename(tmp, stateFilePath()); err != nil {
		log.Printf("[persist] rename error: %v", err)
		return
	}
	log.Printf("[persist] saved %d sessions", len(state.Sessions))
}

func loadSessions() {
	if dataDir == "" {
		return
	}
	data, err := os.ReadFile(stateFilePath())
	if err != nil {
		return
	}
	var state savedState
	if err := json.Unmarshal(data, &state); err != nil {
		log.Printf("[persist] load error: %v", err)
		return
	}
	if state.HealthToken != "" {
		healthToken = state.HealthToken
	}
	sessionsMu.Lock()
	for _, ss := range state.Sessions {
		s := &Session{
			ID:         ss.ID,
			hostSecret: ss.HostSecret,
			terminals:  make(map[string]*Terminal),
			viewers:    make(map[*Client]*UserInfo),
			created:    ss.Created,
		}
		for _, st := range ss.Terminals {
			s.terminals[st.ID] = &Terminal{
				ID: st.ID, Name: st.Name,
				cols: st.Cols, rows: st.Rows,
				X: st.X, Y: st.Y,
				created: ss.Created,
			}
		}
		s.touch()
		sessions[s.ID] = s
	}
	sessionsMu.Unlock()
	log.Printf("[persist] restored %d sessions", len(state.Sessions))
}

func persistLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		saveSessions()
	}
}

func getSession(id string) *Session {
	sessionsMu.RLock()
	defer sessionsMu.RUnlock()
	return sessions[id]
}

func serve404(w http.ResponseWriter, r *http.Request) {
	data, err := webFS.ReadFile("web/404.html")
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store")
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.WriteHeader(http.StatusNotFound)
	html := strings.ReplaceAll(string(data), "{{DOMAIN}}", domain)
	fmt.Fprint(w, html)
}

func drainBody(r *http.Request) {
	io.Copy(io.Discard, io.LimitReader(r.Body, 1<<16))
	r.Body.Close()
}

func deleteSession(id string) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	delete(sessions, id)
}

func handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		serve404(w, r)
		return
	}

	ip := clientIP(r)
	if !sessionLimiter.allow(ip, 0.2, 2) {
		drainBody(r)
		serve404(w, r)
		return
	}

	sessionsMu.RLock()
	sc := len(sessions)
	sessionsMu.RUnlock()
	if sc >= 1000 {
		drainBody(r)
		serve404(w, r)
		return
	}

	var req struct {
		Name       string `json:"name"`
		ID         string `json:"id"`
		HostSecret string `json:"hostSecret"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)

	id := generateID()
	hostSecret := generateSecret()

	if req.ID != "" && validHexID.MatchString(req.ID) && len(req.ID) >= 16 && len(req.ID) <= 64 {
		sessionsMu.RLock()
		_, exists := sessions[req.ID]
		sessionsMu.RUnlock()
		if !exists {
			id = req.ID
		}
	}
	if req.HostSecret != "" && validHexID.MatchString(req.HostSecret) && len(req.HostSecret) >= 16 {
		hostSecret = req.HostSecret
	}

	s := &Session{
		ID:         id,
		hostSecret: hostSecret,
		terminals:  make(map[string]*Terminal),
		viewers:    make(map[*Client]*UserInfo),
		created:    time.Now(),
	}

	termID := shortID()
	termName := "main"
	if req.Name != "" {
		termName = sanitizeName(req.Name)
	}
	s.terminals[termID] = &Terminal{
		ID: termID, Name: termName,
		cols: 80, rows: 24,
		created: time.Now(),
	}

	sessionsMu.Lock()
	sessions[id] = s
	sessionsMu.Unlock()

	log.Printf("[session] created (id=%s…, requested=%v)", id[:8], req.ID != "")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id, "termId": termID, "hostSecret": hostSecret,
	})
}

func handleSessionInfo(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	if i := strings.Index(id, "/"); i >= 0 {
		id = id[:i]
	}
	if !validHexID.MatchString(id) {
		serve404(w, r)
		return
	}
	s := getSession(id)
	if s == nil {
		serve404(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": s.ID,
	})
}

func handleCreateTerminal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		serve404(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/")
	if len(parts) < 2 || !validHexID.MatchString(parts[0]) {
		serve404(w, r)
		return
	}
	s := getSession(parts[0])
	if s == nil {
		serve404(w, r)
		return
	}

	secret := r.Header.Get("X-Host-Secret")
	if !constantTimeEqual(secret, s.hostSecret) {
		drainBody(r)
		serve404(w, r)
		return
	}

	s.mu.RLock()
	tc := len(s.terminals)
	s.mu.RUnlock()
	if tc >= 14 {
		drainBody(r)
		serve404(w, r)
		return
	}

	var req struct{ Name string `json:"name"` }
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
	if req.Name == "" {
		req.Name = fmt.Sprintf("shell-%s", shortID())
	} else {
		req.Name = sanitizeName(req.Name)
	}

	termID := shortID()
	x, y := s.nextPosition()
	t := &Terminal{
		ID: termID, Name: req.Name,
		X: x, Y: y, cols: 80, rows: 24,
		created: time.Now(),
	}
	s.addTerminal(t)
	notifyHost(s, t)
	log.Printf("[session] terminal created")
	s.broadcastShellList()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": termID, "name": req.Name, "x": x, "y": y,
	})
}

func notifyHost(s *Session, t *Terminal) {
	s.mu.RLock()
	hc := s.hostConn
	s.mu.RUnlock()
	if hc != nil {
		msg, _ := json.Marshal(map[string]interface{}{
			"t": "new_term", "tid": t.ID, "n": t.Name,
		})
		hc.send(msg)
	}
}

func handleHostWS(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !generalLimiter.allow(ip+":ws", 2, 10) {
		serve404(w, r)
		return
	}
	if !wsTracker.acquire(ip) {
		serve404(w, r)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/ws/host/")
	parts := strings.SplitN(path, "/", 2)
	sessionID := parts[0]
	termID := ""
	if len(parts) > 1 {
		termID = parts[1]
	}

	if !validHexID.MatchString(sessionID) {
		wsTracker.release(ip)
		serve404(w, r)
		return
	}

	s := getSession(sessionID)
	if s == nil {
		wsTracker.release(ip)
		serve404(w, r)
		return
	}

	secret := r.Header.Get("X-Host-Secret")
	if !constantTimeEqual(secret, s.hostSecret) {
		wsTracker.release(ip)
		serve404(w, r)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		wsTracker.release(ip)
		return
	}
	conn.SetReadLimit(65536)

	client := &Client{conn: conn, id: shortID()}

	if termID == "" {
		s.mu.Lock()
		s.hostConn = client
		s.mu.Unlock()
		log.Printf("[host] control connected")
		handleHostControl(s, client, conn, ip)
		return
	}

	t := s.getTerminal(termID)
	if t == nil {
		wsTracker.release(ip)
		conn.Close()
		return
	}

	t.mu.Lock()
	if t.host != nil {
		t.mu.Unlock()
		wsTracker.release(ip)
		conn.Close()
		return
	}
	t.host = client
	if t.graceTimer != nil {
		t.graceTimer.Stop()
		t.graceTimer = nil
		log.Printf("[host] terminal %s reconnected, grace period cancelled", termID)
	}
	t.mu.Unlock()

	log.Printf("[host] terminal connected")
	s.touch()
	setupPingPong(client, conn)

	defer func() {
		wsTracker.release(ip)
		client.markClosed()
		t.mu.Lock()
		t.host = nil
		t.mu.Unlock()
		conn.Close()

		log.Printf("[host] terminal %s WS disconnected, 120s grace period", termID)
		s.broadcastShellList()

		t.mu.Lock()
		t.graceTimer = time.AfterFunc(120*time.Second, func() {
			cur := s.getTerminal(termID)
			if cur == nil {
				return
			}
			cur.mu.RLock()
			hasHost := cur.host != nil
			cur.mu.RUnlock()
			if hasHost {
				return
			}
			closedMsg, _ := json.Marshal(map[string]interface{}{
				"t": "closed", "tid": termID,
			})
			s.broadcastToViewers(closedMsg)
			s.removeTerminal(termID)
			s.broadcastShellList()
			log.Printf("[host] terminal %s removed after grace period", termID)

			s.mu.Lock()
			tc := len(s.terminals)
			hasControl := s.hostConn != nil
			if tc == 0 && !hasControl {
				vc := len(s.viewers)
				if vc == 0 {
					s.mu.Unlock()
					deleteSession(sessionID)
					log.Printf("[session] removed: no control, no terminals, no viewers")
					return
				}
				s.deleteTimer = time.AfterFunc(10*time.Minute, func() {
					cur := getSession(sessionID)
					if cur == nil {
						return
					}
					cur.mu.RLock()
					hasHost := cur.hostConn != nil
					curTc := len(cur.terminals)
					cur.mu.RUnlock()
					if hasHost || curTc > 0 {
						return
					}
					deleteSession(sessionID)
					log.Printf("[session] removed after grace period (no reconnect)")
				})
				log.Printf("[session] host disconnected, 10min grace period started")
			}
			s.mu.Unlock()
		})
		t.mu.Unlock()
	}()

	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[host] terminal %s read error: %v", termID, err)
			}
			return
		}
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		s.touch()

		if bytes.Contains(msg, []byte(`"keepalive"`)) {
			continue
		}

		if bytes.Contains(msg, []byte(`"resize"`)) {
			var m struct {
				Type string `json:"t"`
				Cols int    `json:"c"`
				Rows int    `json:"r"`
			}
			if json.Unmarshal(msg, &m) == nil && m.Type == "resize" {
				t.mu.Lock()
				t.cols = m.Cols
				t.rows = m.Rows
				t.mu.Unlock()
			}
		}

		s.broadcastTermData(termID, msg)
	}
}

func handleHostControl(s *Session, client *Client, conn *websocket.Conn, ip string) {
	setupPingPong(client, conn)

	defer func() {
		wsTracker.release(ip)
		client.markClosed()
		s.mu.Lock()
		s.hostConn = nil
		tc := len(s.terminals)
		s.mu.Unlock()
		conn.Close()
		log.Printf("[host] control disconnected")

		if tc == 0 {
			s.mu.Lock()
			s.deleteTimer = time.AfterFunc(120*time.Second, func() {
				cur := getSession(s.ID)
				if cur == nil {
					return
				}
				cur.mu.RLock()
				hasHost := cur.hostConn != nil
				curTc := len(cur.terminals)
				cur.mu.RUnlock()
				if hasHost || curTc > 0 {
					return
				}
				deleteSession(s.ID)
				log.Printf("[session] removed after grace period (control never reconnected)")
			})
			s.mu.Unlock()
			log.Printf("[session] control disconnected, 120s grace period started")
		}
	}()

	shells := s.shellList()
	msg, _ := json.Marshal(map[string]interface{}{
		"t": "shells", "shells": shells,
	})
	client.send(msg)

	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[host] control read error: %v", err)
			}
			return
		}
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		s.touch()

		var m struct {
			Type   string `json:"t"`
			TermID string `json:"tid"`
		}
		if json.Unmarshal(msg, &m) != nil {
			continue
		}
		if m.Type == "term_ready" {
			s.broadcastShellList()
		}
	}
}

func handleViewWS(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !generalLimiter.allow(ip+":wsv", 1, 8) {
		serve404(w, r)
		return
	}
	if !wsTracker.acquire(ip) {
		serve404(w, r)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/ws/view/")
	parts := strings.SplitN(path, "/", 2)
	sessionID := parts[0]

	if !validHexID.MatchString(sessionID) {
		wsTracker.release(ip)
		serve404(w, r)
		return
	}

	s := getSession(sessionID)
	if s == nil {
		wsTracker.release(ip)
		serve404(w, r)
		return
	}

	s.mu.RLock()
	vc := len(s.viewers)
	s.mu.RUnlock()
	if vc >= 50 {
		wsTracker.release(ip)
		serve404(w, r)
		return
	}

	userName := sanitizeName(r.URL.Query().Get("name"))

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		wsTracker.release(ip)
		return
	}
	conn.SetReadLimit(65536)

	userID := shortID()
	s.mu.RLock()
	colorIdx := len(s.viewers) % len(userColors)
	s.mu.RUnlock()

	user := &UserInfo{
		ID: userID, Name: userName,
		Color:    userColors[colorIdx],
		JoinedAt: time.Now().Unix(),
	}
	client := &Client{conn: conn, id: userID}
	s.addViewer(client, user)
	s.touch()

	log.Printf("[view] user joined")

	identityMsg, _ := json.Marshal(map[string]interface{}{
		"t": "identity", "u": userID, "n": userName, "color": user.Color,
	})
	client.send(identityMsg)

	shells := s.shellList()
	shellMsg, _ := json.Marshal(map[string]interface{}{
		"t": "shells", "shells": shells,
	})
	client.send(shellMsg)

	s.mu.RLock()
	for _, t := range s.terminals {
		t.replayTo(client)
	}
	s.mu.RUnlock()

	s.broadcastUserUpdate()

	setupViewerPingPong(client, conn)

	defer func() {
		wsTracker.release(ip)
		client.markClosed()
		s.removeViewer(client)
		conn.Close()
		s.broadcastUserUpdate()
		log.Printf("[view] user left")

		s.mu.RLock()
		tc := len(s.terminals)
		hasHost := s.hostConn != nil
		vc := len(s.viewers)
		hasGrace := s.deleteTimer != nil
		s.mu.RUnlock()
		if tc == 0 && !hasHost && vc == 0 && !hasGrace {
			deleteSession(s.ID)
			log.Printf("[session] removed: last viewer left, no host, no terminals")
		}
	}()

	conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[view] read error: %v", err)
			}
			return
		}
		conn.SetReadDeadline(time.Now().Add(20 * time.Second))
		s.touch()

		var m struct {
			Type    string  `json:"t"`
			TermID  string  `json:"tid"`
			Data    string  `json:"d"`
			Name    string  `json:"n"`
			Cols    int     `json:"c"`
			Rows    int     `json:"r"`
			X       float64 `json:"x"`
			Y       float64 `json:"y"`
			Ts      int64   `json:"ts,omitempty"`
			Latency int     `json:"latency,omitempty"`
		}
		if json.Unmarshal(msg, &m) != nil {
			continue
		}

		switch m.Type {
		case "data":
			if t := s.getTerminal(m.TermID); t != nil {
				fwd, _ := json.Marshal(map[string]interface{}{
					"t": "data", "d": m.Data, "u": userID,
				})
				t.sendToHost(fwd)
			}

		case "resize":
			if m.Cols <= 0 || m.Cols > 500 || m.Rows <= 0 || m.Rows > 200 {
				continue
			}
			if t := s.getTerminal(m.TermID); t != nil {
				fwd, _ := json.Marshal(map[string]interface{}{
					"t": "resize", "c": m.Cols, "r": m.Rows,
				})
				t.sendToHost(fwd)
			}

		case "cursor":
			if m.X != m.X || m.Y != m.Y {
				continue
			}
			if m.X > 1e6 || m.X < -1e6 || m.Y > 1e6 || m.Y < -1e6 {
				continue
			}
			s.mu.Lock()
			if u := s.viewers[client]; u != nil {
				u.CursorX = m.X
				u.CursorY = m.Y
			}
			s.mu.Unlock()
			curMsg, _ := json.Marshal(map[string]interface{}{
				"t": "cursor", "u": userID, "x": m.X, "y": m.Y,
				"n": userName, "color": user.Color,
			})
			s.broadcastToViewersExcept(curMsg, client)

		case "ping":
			pongMsg, _ := json.Marshal(map[string]interface{}{
				"t": "pong", "ts": m.Ts,
			})
			client.send(pongMsg)

		case "latency":
			s.mu.Lock()
			if u := s.viewers[client]; u != nil {
				u.Latency = m.Latency
			}
			s.mu.Unlock()

		case "focus":
			s.mu.Lock()
			if u := s.viewers[client]; u != nil {
				u.Focus = m.TermID
			}
			s.mu.Unlock()
			s.broadcastUserUpdate()

		case "move":
			if m.X != m.X || m.Y != m.Y { // NaN check
				continue
			}
			if m.X > 1e6 || m.X < -1e6 || m.Y > 1e6 || m.Y < -1e6 {
				continue
			}
			if t := s.getTerminal(m.TermID); t != nil {
				t.mu.Lock()
				t.X = m.X
				t.Y = m.Y
				t.mu.Unlock()
			}
			moveMsg, _ := json.Marshal(map[string]interface{}{
				"t": "move", "tid": m.TermID, "x": m.X, "y": m.Y,
			})
			s.broadcastToViewersExcept(moveMsg, client)

		case "close":
			if t := s.getTerminal(m.TermID); t != nil {
				t.mu.Lock()
				h := t.host
				t.mu.Unlock()
				if h != nil {
					killMsg, _ := json.Marshal(map[string]interface{}{
						"t": "kill", "tid": m.TermID,
					})
					h.send(killMsg)
					h.markClosed()
					h.conn.Close()
				}
				closedMsg, _ := json.Marshal(map[string]interface{}{
					"t": "closed", "tid": m.TermID,
				})
				s.broadcastToViewersExcept(closedMsg, client)
				s.removeTerminal(m.TermID)
				s.broadcastShellList()
				log.Printf("[session] terminal closed by viewer")
			}

		case "create":
			ip := clientIP(r)
			if !sessionLimiter.allow(ip+":viewcreate", 0.2, 3) {
				continue
			}
			name := m.Name
			if name == "" {
				name = fmt.Sprintf("shell-%s", shortID())
			} else {
				name = sanitizeName(name)
			}
			s.mu.RLock()
			tc := len(s.terminals)
			s.mu.RUnlock()
			if tc >= 14 {
				continue
			}
			termID := shortID()
			x, y := s.nextPosition()
			t := &Terminal{
				ID: termID, Name: name,
				X: x, Y: y, cols: 80, rows: 24,
				created: time.Now(),
			}
			s.addTerminal(t)
			notifyHost(s, t)
			log.Printf("[session] terminal created by viewer")
			s.broadcastShellList()
		}
	}
}

func setupPingPong(client *Client, conn *websocket.Conn) {
	client.touchPong()
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		client.touchPong()
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	conn.SetPingHandler(func(appData string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		client.mu.Lock()
		defer client.mu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		return conn.WriteMessage(websocket.PongMessage, []byte(appData))
	})
	conn.SetCloseHandler(func(code int, text string) error {
		client.markClosed()
		return nil
	})
	// Protocol-level ping every 20s, 3 failures = dead
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		failures := 0
		for range ticker.C {
			if client.isClosed() {
				return
			}
			if err := client.sendPing(); err != nil {
				failures++
				if failures >= 3 {
					client.markClosed()
					conn.Close()
					return
				}
			} else {
				failures = 0
			}
		}
	}()
	// Application-level keepalive every 25s
	go func() {
		keepaliveMsg := []byte(`{"t":"keepalive"}`)
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if client.isClosed() {
				return
			}
			if err := client.send(keepaliveMsg); err != nil {
				return
			}
		}
	}()
}

// setupViewerPingPong handles viewer WebSocket connections.
// Uses both protocol and app-level keepalives for maximum resilience.
func setupViewerPingPong(client *Client, conn *websocket.Conn) {
	client.touchPong()
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		client.touchPong()
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	conn.SetPingHandler(func(appData string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		client.mu.Lock()
		defer client.mu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		return conn.WriteMessage(websocket.PongMessage, []byte(appData))
	})
	conn.SetCloseHandler(func(code int, text string) error {
		client.markClosed()
		return nil
	})
	// Protocol-level ping every 20s, 3 failures = dead
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		failures := 0
		for range ticker.C {
			if client.isClosed() {
				return
			}
			if err := client.sendPing(); err != nil {
				failures++
				if failures >= 3 {
					client.markClosed()
					conn.Close()
					return
				}
			} else {
				failures = 0
			}
		}
	}()
	// Keepalive every 20s as backup
	go func() {
		keepaliveMsg := []byte(`{"t":"keepalive"}`)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if client.isClosed() {
				return
			}
			if err := client.send(keepaliveMsg); err != nil {
				client.markClosed()
				conn.Close()
				return
			}
		}
	}()
}

func handleTerminal(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !generalLimiter.allow(ip+":page", 3, 15) {
		serve404(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/x/")
	if path == "" || path == "/" {
		serve404(w, r)
		return
	}
	parts := strings.Split(path, "/")
	sessionID := parts[0]
	if !validHexID.MatchString(sessionID) {
		serve404(w, r)
		return
	}
	data, err := webFS.ReadFile("web/terminal.html")
	if err != nil {
		serve404(w, r)
		return
	}
	html := strings.ReplaceAll(string(data), "{{DOMAIN}}", domain)
	html = strings.ReplaceAll(html, "{{SESSION_ID}}", sessionID)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	fmt.Fprint(w, html)
}

func ghostReaper() {
	for {
		time.Sleep(30 * time.Second)

		sessionsMu.RLock()
		ids := make([]string, 0, len(sessions))
		for id := range sessions {
			ids = append(ids, id)
		}
		sessionsMu.RUnlock()

		for _, id := range ids {
			s := getSession(id)
			if s == nil {
				continue
			}

			s.mu.RLock()
			deadViewers := make([]*Client, 0)
			for c := range s.viewers {
				if c.isClosed() {
					deadViewers = append(deadViewers, c)
				} else if pa := c.pongAge(); pa > 2*time.Minute && pa != 0 {
					deadViewers = append(deadViewers, c)
				}
			}
			s.mu.RUnlock()

			for _, c := range deadViewers {
				c.markClosed()
				c.conn.Close()
				s.removeViewer(c)
				log.Printf("[reaper] removed ghost viewer")
			}
			if len(deadViewers) > 0 {
				s.broadcastUserUpdate()
			}

			s.mu.RLock()
			hc := s.hostConn
			s.mu.RUnlock()
			if hc != nil && hc.isClosed() {
				s.mu.Lock()
				if s.hostConn == hc {
					s.hostConn = nil
				}
				s.mu.Unlock()
				hc.conn.Close()
				log.Printf("[reaper] removed ghost host control")
			}

			s.mu.RLock()
			termIDs := make([]string, 0, len(s.terminals))
			for tid := range s.terminals {
				termIDs = append(termIDs, tid)
			}
			s.mu.RUnlock()

			for _, tid := range termIDs {
				t := s.getTerminal(tid)
				if t == nil {
					continue
				}
				t.mu.RLock()
				h := t.host
				t.mu.RUnlock()
				if h != nil && h.isClosed() {
					t.mu.Lock()
					t.host = nil
					t.mu.Unlock()
					h.conn.Close()
					log.Printf("[reaper] removed ghost terminal host")
				}
			}

			s.mu.RLock()
			tc := len(s.terminals)
			hasHost := s.hostConn != nil
			vc := len(s.viewers)
			hasGrace := s.deleteTimer != nil
			s.mu.RUnlock()

			if tc == 0 && !hasHost && vc == 0 {
				if hasGrace {
					continue
				}
				s.closeAll()
				deleteSession(id)
				log.Printf("[reaper] removed empty session (no terminals, no host, no viewers)")
				continue
			}

			if !hasHost && vc == 0 && s.idleDuration() > 10*time.Minute {
				s.closeAll()
				deleteSession(id)
				log.Printf("[reaper] removed dead session (no host, no viewers, idle)")
				continue
			}

			if !hasHost && tc == 0 && time.Since(s.created) > 10*time.Minute {
				s.closeAll()
				deleteSession(id)
				log.Printf("[reaper] removed orphan session")
				continue
			}

			if s.idleDuration() > 60*time.Minute {
				log.Printf("[reaper] force-closing idle session")
				s.closeAll()
				deleteSession(id)
			}
		}
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "nginx")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
		cspDomain := strings.SplitN(domain, ":", 2)[0]
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com https://cdn.jsdelivr.net; font-src 'self' https://fonts.gstatic.com; connect-src 'self' wss://"+cspDomain+" wss://"+cspDomain+":* ws://"+cspDomain+" ws://"+cspDomain+":*; img-src 'self' data:; frame-ancestors 'none'")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		w.Header().Set("X-XSS-Protection", "0")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Del("X-Powered-By")
		next.ServeHTTP(w, r)
	})
}

func main() {
	flag.StringVar(&listenAddr, "addr", ":3337", "listen address (IPv4 or dual-stack)")
	flag.StringVar(&listenAddr6, "addr6", "", "additional IPv6 listen address (e.g. [::]:3337)")
	flag.StringVar(&domain, "domain", "pty.minisocket.io", "public domain")
	flag.BoolVar(&trustProxy, "trust-proxy", true, "trust CF-Connecting-IP / X-Forwarded-For headers")
	flag.StringVar(&dataDir, "data", "", "data directory for session persistence (empty = no persistence)")
	flag.Parse()

	if dataDir != "" {
		os.MkdirAll(dataDir, 0700)
		loadSessions()
		go persistLoop()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		log.Printf("[shutdown] saving sessions...")
		saveSessions()
		os.Exit(0)
	}()

	go ghostReaper()

	webContent, _ := fs.Sub(webFS, "web")
	staticHandler := http.FileServer(http.FS(webContent))

	mux := http.NewServeMux()

	mux.HandleFunc("/api/sessions", handleCreateSession)
	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !generalLimiter.allow(ip+":api", 2, 10) {
			serve404(w, r)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
		if strings.HasSuffix(path, "/terminals") && r.Method == http.MethodPost {
			handleCreateTerminal(w, r)
			return
		}
		handleSessionInfo(w, r)
	})

	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Health-Token")
		if !constantTimeEqual(token, healthToken) {
			serve404(w, r)
			return
		}
		sessionsMu.RLock()
		sc := len(sessions)
		var tc, vc int
		for _, s := range sessions {
			s.mu.RLock()
			tc += len(s.terminals)
			vc += len(s.viewers)
			s.mu.RUnlock()
		}
		sessionsMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok": true, "sessions": sc, "terminals": tc, "viewers": vc,
			"uptime": time.Since(startTime).String(),
		})
	})

	mux.HandleFunc("/ws/host/", handleHostWS)
	mux.HandleFunc("/ws/view/", handleViewWS)
	mux.HandleFunc("/x", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x" {
			serve404(w, r)
			return
		}
		handleTerminal(w, r)
	})
	mux.HandleFunc("/x/", handleTerminal)

	mux.HandleFunc("/install.sh", func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !generalLimiter.allow(ip+":dl", 1, 5) {
			serve404(w, r)
			return
		}
		data, _ := webFS.ReadFile("web/install.sh")
		out := strings.ReplaceAll(string(data), "{{DOMAIN}}", domain)
		out = strings.ReplaceAll(out, "{{TOKEN}}", "")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store")
		fmt.Fprint(w, out)
	})

	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !generalLimiter.allow(ip+":dl", 0.5, 3) {
			serve404(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/dl/")
		valid := map[string]bool{
			"minisocketx-linux-amd64": true, "minisocketx-linux-arm64": true,
			"minisocketx-darwin-amd64": true, "minisocketx-darwin-arm64": true,
		}
		if !valid[name] {
			serve404(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename=minisocketx")
		http.ServeFile(w, r, "/var/www/pty.minisocket.io/bin/"+name)
	})

	noCacheStatic := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		staticHandler.ServeHTTP(w, r)
	})
	mux.Handle("/js/", noCacheStatic)
	mux.Handle("/css/", noCacheStatic)
	mux.Handle("/favicon.ico", staticHandler)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			serve404(w, r)
			return
		}
		data, _ := webFS.ReadFile("web/index.html")
		html := strings.ReplaceAll(string(data), "{{DOMAIN}}", domain)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, html)
	})

	handler := securityHeaders(mux)
	log.Printf("Health token: %s...%s", healthToken[:4], healthToken[len(healthToken)-4:])

	if listenAddr6 != "" {
		go func() {
			ln6, err := net.Listen("tcp6", listenAddr6)
			if err != nil {
				log.Printf("IPv6 listen failed: %v", err)
				return
			}
			log.Printf("Server listening on %s (IPv6)", listenAddr6)
			if err := http.Serve(ln6, handler); err != nil {
				log.Printf("IPv6 server error: %v", err)
			}
		}()
	}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Server listening on %s", listenAddr)
	if err := http.Serve(tcpKeepAliveListener{ln.(*net.TCPListener)}, handler); err != nil {
		log.Fatal(err)
	}
}

type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (net.Conn, error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return nil, err
	}
	tc.SetNoDelay(true)
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(30 * time.Second)
	return tc, nil
}
