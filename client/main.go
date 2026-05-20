package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

// dualStackDialer races IPv6 and IPv4 in parallel (Happy Eyeballs style).
// Whichever connects first wins. TCP keepalive enabled on all connections.
func dualStackDialer(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 15 * time.Second,
	}

	type result struct {
		conn net.Conn
		err  error
	}

	ctx6, cancel6 := context.WithCancel(ctx)
	ctx4, cancel4 := context.WithCancel(ctx)
	defer cancel6()
	defer cancel4()

	ch := make(chan result, 2)

	go func() {
		c, e := d.DialContext(ctx6, "tcp6", addr)
		ch <- result{c, e}
	}()

	// Start IPv4 after 200ms head start for IPv6
	go func() {
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx4.Done():
			ch <- result{nil, ctx4.Err()}
			return
		}
		c, e := d.DialContext(ctx4, "tcp4", addr)
		ch <- result{c, e}
	}()

	var firstErr error
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err == nil {
			cancel6()
			cancel4()
			if tc, ok := r.conn.(*net.TCPConn); ok {
				tc.SetKeepAlive(true)
				tc.SetKeepAlivePeriod(15 * time.Second)
			}
			return r.conn, nil
		}
		if firstErr == nil {
			firstErr = r.err
		}
	}
	return nil, firstErr
}

var version = "3.1.0"

// --- Crypto ---

func encrypt(gcm cipher.AEAD, plaintext []byte) string {
	nonce := make([]byte, gcm.NonceSize())
	io.ReadFull(rand.Reader, nonce)
	ct := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(ct)
}

func decrypt(gcm cipher.AEAD, encoded string) ([]byte, error) {
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(data) < ns {
		return nil, fmt.Errorf("ciphertext too short")
	}
	return gcm.Open(nil, data[:ns], data[ns:], nil)
}

func newGCM(key []byte) cipher.AEAD {
	block, err := aes.NewCipher(key)
	if err != nil {
		log.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		log.Fatal(err)
	}
	return gcm
}

// --- Safe WebSocket wrapper ---

type safeConn struct {
	conn     *websocket.Conn
	mu       sync.Mutex
	closed   int32
	closeCh  chan struct{}
	lastPong int64
}

func newSafeConn(conn *websocket.Conn) *safeConn {
	return &safeConn{conn: conn, closeCh: make(chan struct{}), lastPong: time.Now().UnixNano()}
}

func (sc *safeConn) send(msg []byte) error {
	if atomic.LoadInt32(&sc.closed) == 1 {
		return fmt.Errorf("closed")
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return sc.conn.WriteMessage(websocket.TextMessage, msg)
}

func (sc *safeConn) sendPing() error {
	if atomic.LoadInt32(&sc.closed) == 1 {
		return fmt.Errorf("closed")
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return sc.conn.WriteMessage(websocket.PingMessage, nil)
}

func (sc *safeConn) close() {
	if atomic.CompareAndSwapInt32(&sc.closed, 0, 1) {
		close(sc.closeCh)
		sc.mu.Lock()
		sc.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(2*time.Second),
		)
		sc.mu.Unlock()
		sc.conn.Close()
	}
}

func (sc *safeConn) isClosed() bool {
	return atomic.LoadInt32(&sc.closed) == 1
}

// --- TTY Guard ---

type ttyGuard struct {
	fd       int
	oldState *term.State
	mu       sync.Mutex
	restored int32
}

func newTTYGuard(fd int) *ttyGuard {
	return &ttyGuard{fd: fd}
}

func (g *ttyGuard) makeRaw() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	state, err := term.MakeRaw(g.fd)
	if err != nil {
		return err
	}
	g.oldState = state
	atomic.StoreInt32(&g.restored, 0)
	return nil
}

func (g *ttyGuard) restore() {
	if !atomic.CompareAndSwapInt32(&g.restored, 0, 1) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.oldState != nil {
		term.Restore(g.fd, g.oldState)
		g.oldState = nil
	}
}

// --- Terminal Instance ---

type termInstance struct {
	id     string
	name   string
	isMain bool
	ptmx   *os.File
	cmd    *exec.Cmd
	ws     *safeConn
	cancel context.CancelFunc
	done   chan struct{}
}

// --- Telegram ---

func sendTelegram(botToken, chatID, link string, isReconnect bool, httpClient *http.Client) {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	status := "New Session"
	if isReconnect {
		status = "Reconnected"
	}
	msg := fmt.Sprintf("🔗 *MinisocketX %s*\nHost: `%s`\nUser: `%s`\nLink: %s\nTime: %s",
		status, hostname, os.Getenv("USER"), link, time.Now().UTC().Format("2006-01-02 15:04 UTC"))

	vals := url.Values{}
	vals.Set("chat_id", chatID)
	vals.Set("parse_mode", "Markdown")
	vals.Set("text", msg)
	tgURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	req, err := http.NewRequest("POST", tgURL, strings.NewReader(vals.Encode()))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// --- Main ---

func main() {
	serverURL := os.Getenv("SSHX_SERVER")
	if serverURL == "" {
		serverURL = "https://pty.minisocket.io"
	}
	apiToken := ""
	daemonMode := false
	sessionName := ""
	tgBot := os.Getenv("TG_BOT")
	tgChat := os.Getenv("TG_CHAT")
	sessionFile := os.Getenv("MINISOCKETX_SESSION_FILE")

	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--version", "-v":
			fmt.Printf("minisocketx v%s\n", version)
			return
		case "--help", "-h":
			fmt.Println("Usage: minisocketx [options]")
			fmt.Println()
			fmt.Println("Options:")
			fmt.Println("  -v, --version          Show version")
			fmt.Println("  -h, --help             Show help")
			fmt.Println("  -s, --server URL       Server URL")
			fmt.Println("  -t, --token TOKEN      API token (optional)")
			fmt.Println("  -n, --name NAME        Session name")
			fmt.Println("  -d, --daemon           Daemon mode")
			fmt.Println("  --tg-bot TOKEN         Telegram bot token")
			fmt.Println("  --tg-chat ID           Telegram chat ID")
			fmt.Println("  --session-file PATH    Persist session link to file")
			fmt.Println()
			fmt.Println("Environment:")
			fmt.Println("  SSHX_SERVER                Server URL")
			fmt.Println("  TG_BOT                     Telegram bot token")
			fmt.Println("  TG_CHAT                     Telegram chat ID")
			fmt.Println("  MINISOCKETX_SESSION_FILE   Session persistence file")
			return
		case "--server", "-s":
			if i+1 < len(os.Args) {
				serverURL = os.Args[i+1]
				i++
			}
		case "--token", "-t":
			if i+1 < len(os.Args) {
				apiToken = os.Args[i+1]
				i++
			}
		case "--name", "-n":
			if i+1 < len(os.Args) {
				sessionName = os.Args[i+1]
				i++
			}
		case "--daemon", "-d":
			daemonMode = true
		case "--tg-bot":
			if i+1 < len(os.Args) {
				tgBot = os.Args[i+1]
				i++
			}
		case "--tg-chat":
			if i+1 < len(os.Args) {
				tgChat = os.Args[i+1]
				i++
			}
		case "--session-file":
			if i+1 < len(os.Args) {
				sessionFile = os.Args[i+1]
				i++
			}
		}
	}

	tgEnabled := tgBot != "" && tgChat != ""
	if sessionFile == "" {
		homeDir, _ := os.UserHomeDir()
		if homeDir != "" {
			sessionFile = homeDir + "/.minisocketx-session"
		}
	}

	apiURL := serverURL
	wsURL := strings.Replace(serverURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)

	transport := &http.Transport{
		DialContext:         dualStackDialer,
		TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:       10,
		IdleConnTimeout:    30 * time.Second,
		DisableCompression: true,
	}
	httpClient := &http.Client{Timeout: 15 * time.Second, Transport: transport}

	type savedSession struct {
		ID         string `json:"id"`
		TermID     string `json:"termId"`
		HostSecret string `json:"hostSecret"`
		Key        string `json:"key"`
		URL        string `json:"url"`
	}

	var session struct {
		ID         string `json:"id"`
		TermID     string `json:"termId"`
		HostSecret string `json:"hostSecret"`
	}
	var key []byte
	restored := false

	var saved savedSession
	hasSavedSession := false
	if sessionFile != "" {
		data, err := os.ReadFile(sessionFile)
		if err == nil {
			if json.Unmarshal(data, &saved) == nil && saved.ID != "" && saved.Key != "" {
				decodedKey, _ := base64.RawURLEncoding.DecodeString(saved.Key)
				if len(decodedKey) == 32 {
					hasSavedSession = true
					key = decodedKey
				}
			}
		}
	}

	if hasSavedSession {
		maxAttempts := 5
		for attempt := 0; attempt < maxAttempts; attempt++ {
			checkReq, _ := http.NewRequest("GET", apiURL+"/api/sessions/"+saved.ID, nil)
			checkResp, err := httpClient.Do(checkReq)
			if err == nil {
				if checkResp.StatusCode == 200 {
					session.ID = saved.ID
					session.HostSecret = saved.HostSecret
					if saved.TermID != "" {
						session.TermID = saved.TermID
					} else {
						termName := "main"
						if sessionName != "" {
							termName = sessionName
						}
						termBody, _ := json.Marshal(map[string]string{"name": termName})
						termReq, _ := http.NewRequest("POST", apiURL+"/api/sessions/"+saved.ID+"/terminals", strings.NewReader(string(termBody)))
						termReq.Header.Set("Content-Type", "application/json")
						termReq.Header.Set("X-Host-Secret", saved.HostSecret)
						termResp, err := httpClient.Do(termReq)
						if err == nil && termResp.StatusCode == 200 {
							var newTerm struct {
								ID string `json:"id"`
							}
							json.NewDecoder(termResp.Body).Decode(&newTerm)
							termResp.Body.Close()
							session.TermID = newTerm.ID
						} else {
							if termResp != nil {
								termResp.Body.Close()
							}
							checkResp.Body.Close()
							break
						}
					}
					restored = true
					checkResp.Body.Close()
					break
				}
				checkResp.Body.Close()
			}
			if attempt < maxAttempts-1 {
				time.Sleep(3 * time.Second)
			}
		}

		if !restored {
			createBody := map[string]string{
				"id":         saved.ID,
				"hostSecret": saved.HostSecret,
			}
			if sessionName != "" {
				createBody["name"] = sessionName
			}
			b, _ := json.Marshal(createBody)
			req, err := http.NewRequest("POST", apiURL+"/api/sessions", strings.NewReader(string(b)))
			if err != nil {
				log.Fatalf("Failed to create request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			if apiToken != "" {
				req.Header.Set("X-Api-Token", apiToken)
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				log.Fatalf("Failed to connect: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				log.Fatalf("Server returned %d", resp.StatusCode)
			}
			json.NewDecoder(resp.Body).Decode(&session)
			restored = session.ID == saved.ID
		}
	}

	if !hasSavedSession {
		reqBody := "{}"
		if sessionName != "" {
			b, _ := json.Marshal(map[string]string{"name": sessionName})
			reqBody = string(b)
		}
		req, err := http.NewRequest("POST", apiURL+"/api/sessions", strings.NewReader(reqBody))
		if err != nil {
			log.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if apiToken != "" {
			req.Header.Set("X-Api-Token", apiToken)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			log.Fatalf("Failed to connect: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			log.Fatalf("Server returned %d", resp.StatusCode)
		}
		json.NewDecoder(resp.Body).Decode(&session)

		key = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			log.Fatalf("Failed to generate encryption key: %v", err)
		}
	}

	gcm := newGCM(key)
	keyStr := base64.RawURLEncoding.EncodeToString(key)
	shareURL := fmt.Sprintf("%s/x/%s#%s", serverURL, session.ID, keyStr)

	if sessionFile != "" {
		saved := savedSession{
			ID: session.ID, TermID: session.TermID,
			HostSecret: session.HostSecret, Key: keyStr, URL: shareURL,
		}
		data, _ := json.Marshal(saved)
		os.WriteFile(sessionFile, data, 0600)
	}

	if tgEnabled {
		go sendTelegram(tgBot, tgChat, shareURL, restored, httpClient)
		if daemonMode {
			log.Println("Session active. Link sent to Telegram.")
		} else {
			fmt.Println()
			fmt.Println("  \033[1;36mminisocketx\033[0m v" + version + " — shared terminal session")
			fmt.Println()
			fmt.Println("  \033[2mLink sent to Telegram.\033[0m")
			fmt.Println("  \033[2mPress Ctrl+D or type 'exit' to end.\033[0m")
			fmt.Println()
		}
	} else if daemonMode {
		log.Println("Session active. Link saved to " + sessionFile)
	} else {
		fmt.Println()
		fmt.Println("  \033[1;36mminisocketx\033[0m v" + version + " — shared terminal session")
		fmt.Println()
		fmt.Printf("  \033[1mLink:\033[0m  \033[4;34m%s\033[0m\n", shareURL)
		fmt.Println()
		fmt.Println("  \033[2mShare this link. Others can view and create terminals.\033[0m")
		fmt.Println("  \033[2mPress Ctrl+D or type 'exit' to end.\033[0m")
		fmt.Println()
	}

	// Root context for entire session
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		NetDialContext:   dualStackDialer,
		TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12},
	}
	wsHeaders := http.Header{}
	wsHeaders.Set("X-Host-Secret", session.HostSecret)

	// Control WS with retry
	var controlWS *safeConn
	for attempt := 0; attempt < 3; attempt++ {
		conn, _, err := dialer.Dial(wsURL+"/ws/host/"+session.ID, wsHeaders)
		if err == nil {
			controlWS = newSafeConn(conn)
			break
		}
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * time.Second)
		}
	}
	if controlWS == nil {
		log.Fatal("Control channel failed after retries")
	}

	setupClientPingPong := func(sc *safeConn) {
		sc.conn.SetPongHandler(func(string) error {
			atomic.StoreInt64(&sc.lastPong, time.Now().UnixNano())
			sc.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			return nil
		})
		sc.conn.SetPingHandler(func(appData string) error {
			atomic.StoreInt64(&sc.lastPong, time.Now().UnixNano())
			sc.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			sc.mu.Lock()
			defer sc.mu.Unlock()
			sc.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
			return sc.conn.WriteMessage(websocket.PongMessage, []byte(appData))
		})
		atomic.StoreInt64(&sc.lastPong, time.Now().UnixNano())
		sc.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	}

	setupClientPingPong(controlWS)

	shellPath := os.Getenv("SHELL")
	if shellPath == "" {
		shellPath = "/bin/bash"
	}

	terminals := make(map[string]*termInstance)
	var termMu sync.Mutex
	mainDone := make(chan struct{})

	// TTY guard (safe restore on crash/signal)
	var ttyG *ttyGuard
	stdinFd := int(os.Stdin.Fd())
	isTerminal := !daemonMode && term.IsTerminal(stdinFd)

	// Signal handler - MUST be set up BEFORE MakeRaw
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	if isTerminal {
		signal.Notify(sigCh, syscall.SIGWINCH)
	}

	// Cleanup function
	cleanup := func() {
		rootCancel()
		if ttyG != nil {
			ttyG.restore()
		}
		controlWS.close()
		termMu.Lock()
		for _, ti := range terminals {
			if ti.cancel != nil {
				ti.cancel()
			}
			if ti.cmd != nil && ti.cmd.Process != nil {
				ti.cmd.Process.Signal(syscall.SIGHUP)
			}
			if ti.ptmx != nil {
				ti.ptmx.Close()
			}
			if ti.ws != nil {
				ti.ws.close()
			}
		}
		termMu.Unlock()
	}

	// spawnTerminal: PTY lifecycle is decoupled from WS — PTY survives WS reconnects
	spawnTerminal := func(ctx context.Context, termID, name string, isMain bool) {
		termCtx, termCancel := context.WithCancel(ctx)
		done := make(chan struct{})
		defer close(done)
		defer termCancel()

		// Phase 1: Initial WS dial (must succeed to start PTY)
		var initialWS *safeConn
		for attempt := 0; attempt < 3; attempt++ {
			if termCtx.Err() != nil {
				return
			}
			conn, _, err := dialer.Dial(wsURL+"/ws/host/"+session.ID+"/"+termID, wsHeaders)
			if err == nil {
				initialWS = newSafeConn(conn)
				break
			}
			if attempt < 2 {
				select {
				case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
				case <-termCtx.Done():
					return
				}
			}
		}
		if initialWS == nil && isMain && restored {
			termName := name
			if termName == "" {
				termName = "main"
			}
			termBody, _ := json.Marshal(map[string]string{"name": termName})
			termReq, _ := http.NewRequest("POST", apiURL+"/api/sessions/"+session.ID+"/terminals", strings.NewReader(string(termBody)))
			termReq.Header.Set("Content-Type", "application/json")
			termReq.Header.Set("X-Host-Secret", session.HostSecret)
			termResp, err := httpClient.Do(termReq)
			if err == nil && termResp.StatusCode == 200 {
				var newTerm struct{ ID string `json:"id"` }
				json.NewDecoder(termResp.Body).Decode(&newTerm)
				termResp.Body.Close()
				termID = newTerm.ID
				session.TermID = termID
				if sessionFile != "" {
					saved := savedSession{ID: session.ID, TermID: termID, HostSecret: session.HostSecret, Key: base64.RawURLEncoding.EncodeToString(key), URL: fmt.Sprintf("%s/x/%s#%s", serverURL, session.ID, base64.RawURLEncoding.EncodeToString(key))}
					data, _ := json.Marshal(saved)
					os.WriteFile(sessionFile, data, 0600)
				}
				conn, _, err := dialer.Dial(wsURL+"/ws/host/"+session.ID+"/"+termID, wsHeaders)
				if err == nil {
					initialWS = newSafeConn(conn)
				}
			} else if termResp != nil {
				termResp.Body.Close()
			}
		}
		if initialWS == nil {
			return
		}

		// Phase 2: Start PTY once — lives independently of WS
		cmd := exec.Command(shellPath)
		cmd.Env = append(os.Environ(),
			"TERM=xterm-256color",
			"MINISOCKETX_SESSION="+session.ID,
			"MINISOCKETX_TERMINAL="+termID,
		)

		ptmx, err := pty.Start(cmd)
		if err != nil {
			initialWS.close()
			return
		}

		cmdDone := make(chan error, 1)
		go func() { cmdDone <- cmd.Wait() }()

		killShell := func() {
			cmd.Process.Signal(syscall.SIGHUP)
			select {
			case <-cmdDone:
			case <-time.After(3 * time.Second):
				cmd.Process.Kill()
				<-cmdDone
			}
		}

		ti := &termInstance{
			id: termID, name: name, isMain: isMain,
			ptmx: ptmx, cmd: cmd, ws: initialWS,
			cancel: termCancel, done: done,
		}

		termMu.Lock()
		terminals[termID] = ti
		termMu.Unlock()

		defer func() {
			ptmx.Close()
			termMu.Lock()
			delete(terminals, termID)
			termMu.Unlock()
			if isMain {
				select {
				case <-mainDone:
				default:
					close(mainDone)
				}
			}
		}()

		// Set initial size
		if isMain && isTerminal {
			if size, err := pty.GetsizeFull(os.Stdin); err == nil {
				pty.Setsize(ptmx, size)
				data, _ := json.Marshal(map[string]interface{}{"t": "resize", "c": int(size.Cols), "r": int(size.Rows)})
				initialWS.send(data)
			}
		} else {
			pty.Setsize(ptmx, &pty.Winsize{Rows: 24, Cols: 80})
			data, _ := json.Marshal(map[string]interface{}{"t": "resize", "c": 80, "r": 24})
			initialWS.send(data)
		}

		// Notify control channel
		readyMsg, _ := json.Marshal(map[string]interface{}{"t": "term_ready", "tid": termID})
		controlWS.send(readyMsg)

		// stdin → PTY (independent of WS, never restarts)
		if isMain && !daemonMode && isTerminal {
			go func() {
				buf := make([]byte, 4096)
				for {
					select {
					case <-termCtx.Done():
						return
					default:
					}
					n, err := os.Stdin.Read(buf)
					if err != nil {
						return
					}
					if _, err := ptmx.Write(buf[:n]); err != nil {
						return
					}
				}
			}()
		}

		// Phase 3: Shared WS pointer for PTY→WS goroutine
		var wsMu sync.RWMutex
		var activeWS *safeConn = initialWS

		setActiveWS := func(ws *safeConn) {
			wsMu.Lock()
			activeWS = ws
			wsMu.Unlock()
			termMu.Lock()
			if t, ok := terminals[termID]; ok {
				t.ws = ws
			}
			termMu.Unlock()
		}

		// PTY → WS + stdout (runs for entire PTY lifetime, tolerates WS gaps)
		go func() {
			buf := make([]byte, 32768)
			for {
				n, err := ptmx.Read(buf)
				if err != nil {
					return
				}
				if isMain && !daemonMode {
					os.Stdout.Write(buf[:n])
				}
				wsMu.RLock()
				ws := activeWS
				wsMu.RUnlock()
				if ws != nil && !ws.isClosed() {
					enc := encrypt(gcm, buf[:n])
					data, _ := json.Marshal(map[string]interface{}{"t": "data", "d": enc})
					ws.send(data)
				}
			}
		}()

		// Phase 4: WS connection loop with auto-reconnect
		tws := initialWS
		for {
			ws := tws
			setupClientPingPong(ws)

			if isMain && isTerminal {
				if size, err := pty.GetsizeFull(os.Stdin); err == nil {
					data, _ := json.Marshal(map[string]interface{}{"t": "resize", "c": int(size.Cols), "r": int(size.Rows)})
					ws.send(data)
				}
			}

			// WS → PTY reader (restarts with each WS connection)
			wsDone := make(chan struct{})
			go func() {
				defer close(wsDone)
				for {
					_, raw, err := ws.conn.ReadMessage()
					if err != nil {
						log.Printf("Terminal WS %s read error: %v", termID, err)
						return
					}
					var msg struct {
						Type string `json:"t"`
						Data string `json:"d"`
						Cols int    `json:"c"`
						Rows int    `json:"r"`
					}
					if json.Unmarshal(raw, &msg) != nil {
						continue
					}
					switch msg.Type {
					case "data":
						if pt, err := decrypt(gcm, msg.Data); err == nil {
							ptmx.Write(pt)
						}
					case "resize":
						if msg.Cols > 0 && msg.Rows > 0 {
							pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(msg.Rows), Cols: uint16(msg.Cols)})
						}
					}
				}
			}()

			// Heartbeat with pong watchdog (restarts with each WS connection)
			go func() {
				pingTicker := time.NewTicker(20 * time.Second)
				keepaliveTicker := time.NewTicker(25 * time.Second)
				healthCheck := time.NewTicker(10 * time.Second)
				defer pingTicker.Stop()
				defer keepaliveTicker.Stop()
				defer healthCheck.Stop()
				keepaliveMsg := []byte(`{"t":"keepalive"}`)
				for {
					select {
					case <-pingTicker.C:
						if ws.sendPing() != nil {
							return
						}
					case <-keepaliveTicker.C:
						if ws.send(keepaliveMsg) != nil {
							return
						}
					case <-healthCheck.C:
						lp := time.Unix(0, atomic.LoadInt64(&ws.lastPong))
						if time.Since(lp) > 45*time.Second {
							log.Printf("Terminal %s: no pong for %v, forcing reconnect", termID, time.Since(lp).Round(time.Second))
							ws.close()
							return
						}
					case <-termCtx.Done():
						return
					case <-ws.closeCh:
						return
					case <-wsDone:
						return
					}
				}
			}()

			// Wait for WS disconnect, shell exit, or context cancel
			select {
			case <-wsDone:
				ws.close()
				setActiveWS(nil)
			case <-cmdDone:
				ws.close()
				return
			case <-termCtx.Done():
				ws.close()
				killShell()
				return
			}

			// Reconnect loop — never give up while PTY is alive
			log.Printf("Terminal WS %s disconnected, reconnecting...", termID)
			backoff := 500 * time.Millisecond
			var newConn *websocket.Conn
			for attempt := 0; ; attempt++ {
				select {
				case <-cmdDone:
					return
				case <-termCtx.Done():
					killShell()
					return
				case <-time.After(backoff):
				}
				conn, _, err := dialer.Dial(wsURL+"/ws/host/"+session.ID+"/"+termID, wsHeaders)
				if err == nil {
					newConn = conn
					break
				}
				if attempt%20 == 19 {
					log.Printf("Terminal WS %s reconnect attempt %d failed: %v", termID, attempt+1, err)
				}
				if backoff < 10*time.Second {
					backoff = time.Duration(float64(backoff) * 1.5)
				}
			}

			tws = newSafeConn(newConn)
			setActiveWS(tws)
			log.Printf("Terminal WS %s reconnected after disconnect", termID)
		}
	}

	// Put terminal in raw mode AFTER signal handler is set up
	if isTerminal {
		ttyG = newTTYGuard(stdinFd)
		if err := ttyG.makeRaw(); err != nil {
			log.Fatalf("Raw mode failed: %v", err)
		}
	}

	// Start main terminal
	go spawnTerminal(rootCtx, session.TermID, "main", true)

	// Control channel with auto-reconnect
	go func() {
		backoff := time.Second
		for {
			if rootCtx.Err() != nil {
				return
			}

			// Reader goroutine
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				for {
					_, raw, err := controlWS.conn.ReadMessage()
					if err != nil {
						return
					}
					var msg struct {
						Type   string `json:"t"`
						TermID string `json:"tid"`
						Name   string `json:"n"`
					}
					if json.Unmarshal(raw, &msg) != nil {
						continue
					}
					if msg.Type == "new_term" && msg.TermID != "" {
						n := msg.Name
						if n == "" {
							n = msg.TermID
						}
						go spawnTerminal(rootCtx, msg.TermID, n, false)
					}
				}
			}()

			// Heartbeat with pong watchdog
			go func() {
				pingTicker := time.NewTicker(20 * time.Second)
				keepaliveTicker := time.NewTicker(25 * time.Second)
				healthCheck := time.NewTicker(10 * time.Second)
				defer pingTicker.Stop()
				defer keepaliveTicker.Stop()
				defer healthCheck.Stop()
				keepaliveMsg := []byte(`{"t":"keepalive"}`)
				for {
					select {
					case <-pingTicker.C:
						if controlWS.sendPing() != nil {
							return
						}
					case <-keepaliveTicker.C:
						if controlWS.send(keepaliveMsg) != nil {
							return
						}
					case <-healthCheck.C:
						lp := time.Unix(0, atomic.LoadInt64(&controlWS.lastPong))
						if time.Since(lp) > 45*time.Second {
							log.Printf("Control: no pong for %v, forcing reconnect", time.Since(lp).Round(time.Second))
							controlWS.close()
							return
						}
					case <-rootCtx.Done():
						return
					case <-readDone:
						return
					}
				}
			}()

			<-readDone
			controlWS.close()

			if rootCtx.Err() != nil {
				return
			}
			log.Printf("Control WS disconnected, reconnecting in %v...", backoff)

			select {
			case <-time.After(backoff):
			case <-rootCtx.Done():
				return
			}

			// Reconnect — never give up
			var newConn *websocket.Conn
			reconBackoff := 500 * time.Millisecond
			for attempt := 0; ; attempt++ {
				if rootCtx.Err() != nil {
					return
				}
				conn, _, err := dialer.Dial(wsURL+"/ws/host/"+session.ID, wsHeaders)
				if err == nil {
					newConn = conn
					break
				}
				if attempt%20 == 19 {
					log.Printf("Control WS reconnect attempt %d failed: %v", attempt+1, err)
				}
				wait := reconBackoff
				if reconBackoff < 10*time.Second {
					reconBackoff = time.Duration(float64(reconBackoff) * 1.5)
				}
				select {
				case <-time.After(wait):
				case <-rootCtx.Done():
					return
				}
			}

			controlWS = newSafeConn(newConn)
			setupClientPingPong(controlWS)
			backoff = time.Second
			log.Printf("Control WS reconnected")
		}
	}()

	// Signal + event loop
	go func() {
		for sig := range sigCh {
			switch sig {
			case syscall.SIGWINCH:
				termMu.Lock()
				ti := terminals[session.TermID]
				termMu.Unlock()
				if ti == nil || !isTerminal {
					continue
				}
				size, err := pty.GetsizeFull(os.Stdin)
				if err != nil {
					continue
				}
				pty.Setsize(ti.ptmx, size)
				data, _ := json.Marshal(map[string]interface{}{"t": "resize", "c": int(size.Cols), "r": int(size.Rows)})
				ti.ws.send(data)

			case syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP:
				cleanup()
				fmt.Println("\r\n  \033[2mSession ended.\033[0m")
				os.Exit(0)
			}
		}
	}()

	// Wait for exit condition
	if daemonMode {
		<-rootCtx.Done()
	} else {
		<-mainDone
	}

	cleanup()
	fmt.Println("\r\n  \033[2mSession ended.\033[0m")
}
