# MinisocketX

Encrypted terminal sharing over WebSocket. Share a live shell session with anyone through a browser — no SSH, no port forwarding, no credentials to manage.

## How It Works

```
┌──────────┐    WebSocket (AES-256-GCM)    ┌──────────┐    WebSocket    ┌─────────┐
│  Client   │◄────────────────────────────►│  Server   │◄──────────────►│ Browser │
│  (host)   │   encrypted pty I/O          │  (relay)  │   decrypted    │ (viewer)│
└──────────┘                               └──────────┘   in-browser    └─────────┘
```

The client spawns a PTY, encrypts every byte with AES-256-GCM, and streams it to the server over WebSocket. The server relays ciphertext to connected browsers, which decrypt client-side using a key in the URL fragment (never sent to the server). The server sees only encrypted traffic.

## Quick Start

One command on the host machine:

```bash
curl -sSL https://pty.minisocket.io/install.sh | bash
```

This downloads the client, starts a daemon, and prints a shareable URL. Open it in any browser to view the terminal.

For Telegram notifications when the session starts:

```bash
curl -sSL https://pty.minisocket.io/install.sh | TG_BOT=<token> TG_CHAT=<id> bash
```

## Features

- **End-to-end encryption** — AES-256-GCM, key derived per-session, never leaves the client/browser
- **Zero config** — one curl command, works on Linux and macOS
- **Daemon mode** — runs in background with session persistence and auto-reconnect
- **Static binaries** — no runtime dependencies, CGO disabled
- **Installer handles everything** — root/non-root, systemd/cron, noexec filesystem bypass via memfd
- **Session resume** — daemon survives disconnects, viewers rejoin automatically
- **Dual-stack** — Happy Eyeballs dialing, IPv4 + IPv6
- **Telegram integration** — optional session URL notification via bot

## Architecture

| Component | Description |
|-----------|-------------|
| `main.go` | Server — WebSocket relay, session management, embedded web assets |
| `client/main.go` | Client daemon — PTY, encryption, reconnect logic |
| `web/install.sh` | Installer — embedded in server binary via `go:embed` |
| `web/terminal.html` | Browser viewer — xterm.js, client-side AES decryption |
| `web/index.html` | Landing page |

### Server

The server is a single Go binary that:
- Manages sessions (create, join, heartbeat, expiry)
- Relays encrypted WebSocket frames between host and viewers
- Serves the web UI and installer script (embedded via `go:embed`)
- Serves client binaries from `/dl/` for the installer
- Handles graceful shutdown and session cleanup

### Client

The client:
- Spawns a PTY running the user's shell
- Generates a random AES-256 key and session credentials
- Connects to the server via WebSocket with automatic reconnect
- Encrypts all PTY output before sending, decrypts input before writing
- Writes a session file (`/var/lib/minisocketx/session.json` as root, `~/.minisocketx-session` as user)
- Supports `--daemon` mode with PID file management

### Encryption

Each session generates a fresh 256-bit key. Terminal I/O is encrypted with AES-256-GCM using unique nonces. The key is placed in the URL fragment (`#key=...`), which browsers never send to the server. The server only relays opaque ciphertext.

## Build

Requires Go 1.22+.

```bash
# Server only
make server

# Client for current platform
make client

# All platforms (linux/darwin, amd64/arm64) + server
make build-all

# Deploy to production
make deploy

# Local dev server
make run
```

## Deploy

The `make deploy` target:
1. Builds the server and all client binaries
2. Uploads the server binary and restarts the systemd service
3. Uploads client binaries to the download directory

### Production Setup

```bash
# Copy systemd unit
cp deploy/minisocketx.service /etc/systemd/system/
systemctl enable --now minisocketx

# nginx reverse proxy (WebSocket + binary downloads)
cp deploy/nginx.conf /etc/nginx/sites-available/minisocketx
ln -s /etc/nginx/sites-available/minisocketx /etc/nginx/sites-enabled/
nginx -t && systemctl reload nginx
```

The server listens on `:3337`. Put nginx in front with TLS termination. Use `proxy_buffering off` for WebSocket paths and `proxy_buffering on` for `/dl/` (binary downloads).

## Configuration

### Server flags

| Flag | Default | Description |
|------|---------|-------------|
| `-addr` | `:3337` | Listen address |
| `-addr6` | | IPv6 listen address |
| `-domain` | `localhost:3337` | Public domain for URLs |
| `-trust-proxy` | `false` | Trust X-Forwarded-For headers |
| `-data` | | Data directory for persistence |

### Client flags

| Flag | Default | Description |
|------|---------|-------------|
| `-server` | | Server WebSocket URL |
| `-shell` | `$SHELL` or `/bin/bash` | Shell to spawn |
| `-daemon` | `false` | Run as background daemon |
| `-session-file` | auto | Path to session state file |
| `-tg-bot` | | Telegram bot token for notifications |
| `-tg-chat` | | Telegram chat ID for notifications |
| `-rows` / `-cols` | `40` / `120` | Terminal dimensions |

### Installer environment variables

| Variable | Description |
|----------|-------------|
| `TG_BOT` | Telegram bot token |
| `TG_CHAT` | Telegram chat ID |
| `MINISOCKETX_SESSION_FILE` | Custom session file path |

## Dependencies

- [gorilla/websocket](https://github.com/gorilla/websocket) — WebSocket protocol
- [creack/pty](https://github.com/creack/pty) — PTY allocation
- [golang.org/x/term](https://pkg.go.dev/golang.org/x/term) — Terminal state management

## Project Structure

```
minisocketx/
├── main.go                          # Server (1800 LOC)
├── client/main.go                   # Client daemon (1100 LOC)
├── web/
│   ├── install.sh                   # Installer script (embedded)
│   ├── index.html                   # Landing page
│   ├── terminal.html                # Browser terminal viewer
│   └── 404.html
├── deploy/
│   ├── minisocketx.service          # systemd unit (server)
│   ├── minisocketx-client.service   # systemd unit (client)
│   ├── nginx.conf                   # nginx reverse proxy config
│   └── apache-vhost.conf            # Apache alternative
├── Dockerfile
├── Makefile
├── go.mod
└── go.sum
```

## License

Private.
