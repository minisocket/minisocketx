(function () {
    'use strict';

    var secret = location.hash.slice(1);
    if (!secret) {
        document.getElementById('overlay-text').textContent = 'No encryption key in URL.';
        return;
    }

    // --- Crypto ---
    function b64d(s) {
        s = s.replace(/-/g, '+').replace(/_/g, '/');
        while (s.length % 4) s += '=';
        var b = atob(s), a = new Uint8Array(b.length);
        for (var i = 0; i < b.length; i++) a[i] = b.charCodeAt(i);
        return a;
    }
    function b64e(a) {
        var b = '';
        for (var i = 0; i < a.length; i++) b += String.fromCharCode(a[i]);
        return btoa(b).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
    }
    function importKey(k) {
        return crypto.subtle.importKey('raw', b64d(k), { name: 'AES-GCM' }, false, ['encrypt', 'decrypt']);
    }
    function decrypt(key, d) {
        var raw = b64d(d);
        return crypto.subtle.decrypt({ name: 'AES-GCM', iv: raw.slice(0, 12) }, key, raw.slice(12))
            .then(function (buf) { return new Uint8Array(buf); });
    }
    function encrypt(key, pt) {
        var iv = crypto.getRandomValues(new Uint8Array(12));
        return crypto.subtle.encrypt({ name: 'AES-GCM', iv: iv }, key, pt)
            .then(function (ct) {
                var r = new Uint8Array(12 + ct.byteLength);
                r.set(iv);
                r.set(new Uint8Array(ct), 12);
                return b64e(r);
            });
    }

    // --- State ---
    var cryptoKey = null;
    var userName = '';
    var myUserId = '';
    var myColor = '';
    var ws = null;
    var shells = {};
    var focusedId = null;
    var users = {};

    // Canvas transform
    var panX = 0, panY = 0, zoom = 1.0;
    var MIN_ZOOM = 0.15, MAX_ZOOM = 3.0;
    var isPanning = false, panStartX = 0, panStartY = 0, panStartPanX = 0, panStartPanY = 0;

    // Cursor throttle
    var lastCursorSend = 0;
    var CURSOR_INTERVAL = 80;

    // Latency measurement — EMA smoothed, high-precision
    var currentLatency = 0;
    var smoothedLatency = 0;
    var EMA_ALPHA = 0.3;
    var pingInterval = null;
    var fastPingInterval = null;
    var pendingPingTs = 0;
    var pendingPingPerf = 0;
    var PING_INTERVAL = 5000;
    var latencyHistory = [];
    var LATENCY_HISTORY_SIZE = 10;
    var jitter = 0;

    // DOM
    var viewport = document.getElementById('viewport');
    var canvas = document.getElementById('canvas');
    var grid = document.getElementById('grid');
    var cursorLayer = document.getElementById('cursor-layer');
    var statusEl = document.getElementById('status');
    var overlayEl = document.getElementById('overlay');
    var sessionEl = document.getElementById('session-id');
    var nameModal = document.getElementById('name-modal');
    var nameInput = document.getElementById('name-input');
    var nameBtn = document.getElementById('name-btn');
    var newShellBtn = document.getElementById('new-shell-btn');
    var zoomFitBtn = document.getElementById('zoom-fit-btn');
    var zoomLevelEl = document.getElementById('zoom-level');
    var userAvatarsEl = document.getElementById('user-avatars');
    var viewersEl = document.getElementById('viewers');

    sessionEl.textContent = SESSION_ID.slice(0, 8);

    var THEME = {
        background: '#0f1319', foreground: '#d0d7de', cursor: '#58a6ff',
        cursorAccent: '#0f1319',
        selectionBackground: '#264f78', selectionForeground: '#ffffff',
        selectionInactiveBackground: '#264f7844',
        black: '#484f58', red: '#ff7b72', green: '#3fb950', yellow: '#d29922',
        blue: '#58a6ff', magenta: '#bc8cff', cyan: '#39c5cf', white: '#b1bac4',
        brightBlack: '#6e7681', brightRed: '#ffa198', brightGreen: '#56d364',
        brightYellow: '#e3b341', brightBlue: '#79c0ff', brightMagenta: '#d2a8ff',
        brightCyan: '#56d4dd', brightWhite: '#f0f6fc'
    };

    function esc(s) { var d = document.createElement('div'); d.textContent = s || ''; return d.innerHTML; }
    function setStatus(t, c) { statusEl.textContent = t; statusEl.className = 'status ' + c; }
    function wsSend(obj) { if (ws && ws.readyState === 1) ws.send(JSON.stringify(obj)); }

    // --- Canvas Transform ---
    function updateTransform() {
        canvas.style.zoom = zoom;
        canvas.style.transform = 'translate(' + (panX * zoom) + 'px,' + (panY * zoom) + 'px)';
        grid.style.backgroundSize = (40 * zoom) + 'px ' + (40 * zoom) + 'px';
        grid.style.backgroundPosition = ((viewport.clientWidth / 2 + panX * zoom)) + 'px ' +
            ((viewport.clientHeight / 2 + panY * zoom)) + 'px';
        zoomLevelEl.textContent = Math.round(zoom * 100) + '%';
    }

    function screenToCanvas(sx, sy) {
        var rect = viewport.getBoundingClientRect();
        var cx = (sx - rect.left - rect.width / 2) / zoom - panX;
        var cy = (sy - rect.top - rect.height / 2) / zoom - panY;
        return { x: cx, y: cy };
    }

    function fitToShells() {
        var ids = Object.keys(shells);
        if (ids.length === 0) {
            panX = 0; panY = 0; zoom = 1.0;
            updateTransform();
            return;
        }
        var minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
        ids.forEach(function (id) {
            var s = shells[id];
            var el = s.el;
            var w = el ? el.offsetWidth : 720;
            var h = el ? el.offsetHeight : 480;
            if (s.x < minX) minX = s.x;
            if (s.y < minY) minY = s.y;
            if (s.x + w > maxX) maxX = s.x + w;
            if (s.y + h > maxY) maxY = s.y + h;
        });
        var bw = maxX - minX + 100;
        var bh = maxY - minY + 100;
        var vw = viewport.clientWidth;
        var vh = viewport.clientHeight - 50;
        zoom = Math.min(vw / bw, vh / bh, 1.5);
        zoom = Math.max(MIN_ZOOM, Math.min(MAX_ZOOM, zoom));
        panX = -(minX + (maxX - minX) / 2);
        panY = -(minY + (maxY - minY) / 2);
        updateTransform();
    }

    // --- Shell (Terminal Window) ---
    function createShellElement(shell) {
        showEmptyHint(false);
        var el = document.createElement('div');
        el.className = 'shell';
        el.dataset.id = shell.id;
        el.style.transform = 'translate(' + shell.x + 'px,' + shell.y + 'px)';
        el.style.width = '720px';
        el.style.height = '480px';

        // macOS-style traffic light header
        var header = document.createElement('div');
        header.className = 'shell-header';

        var trafficLights = document.createElement('div');
        trafficLights.className = 'traffic-lights';

        var closeBtn = document.createElement('button');
        closeBtn.className = 'tl-btn tl-close';
        closeBtn.title = 'Close terminal';
        closeBtn.innerHTML = '<svg width="6" height="6" viewBox="0 0 6 6"><path d="M0.5 0.5L5.5 5.5M5.5 0.5L0.5 5.5" stroke="currentColor" stroke-width="1.2" stroke-linecap="round"/></svg>';
        trafficLights.appendChild(closeBtn);

        var minimizeBtn = document.createElement('button');
        minimizeBtn.className = 'tl-btn tl-minimize';
        minimizeBtn.title = 'Minimize';
        minimizeBtn.innerHTML = '<svg width="6" height="1" viewBox="0 0 6 1"><line x1="0.5" y1="0.5" x2="5.5" y2="0.5" stroke="currentColor" stroke-width="1.2" stroke-linecap="round"/></svg>';
        trafficLights.appendChild(minimizeBtn);

        var maximizeBtn = document.createElement('button');
        maximizeBtn.className = 'tl-btn tl-maximize';
        maximizeBtn.title = 'Maximize';
        maximizeBtn.innerHTML = '<svg width="6" height="6" viewBox="0 0 6 6"><rect x="0.5" y="0.5" width="5" height="5" rx="0.5" stroke="currentColor" stroke-width="1" fill="none"/></svg>';
        trafficLights.appendChild(maximizeBtn);

        header.appendChild(trafficLights);

        var titleArea = document.createElement('div');
        titleArea.className = 'shell-title-area';

        var dot = document.createElement('span');
        dot.className = 'shell-dot' + (shell.active ? ' on' : '');
        titleArea.appendChild(dot);

        var title = document.createElement('span');
        title.className = 'shell-title';
        title.textContent = shell.name || shell.id;
        titleArea.appendChild(title);

        header.appendChild(titleArea);

        var headerRight = document.createElement('div');
        headerRight.className = 'shell-header-right';
        var termId = document.createElement('span');
        termId.className = 'shell-tid';
        termId.textContent = shell.id;
        headerRight.appendChild(termId);
        header.appendChild(headerRight);

        el.appendChild(header);

        var body = document.createElement('div');
        body.className = 'shell-body';
        el.appendChild(body);

        var resizeHandle = document.createElement('div');
        resizeHandle.className = 'shell-resize';
        el.appendChild(resizeHandle);

        canvas.insertBefore(el, cursorLayer);
        el.classList.add('appearing');
        setTimeout(function () { el.classList.remove('appearing'); }, 400);

        var term = new Terminal({
            cursorBlink: true, cursorStyle: 'bar', cursorWidth: 2,
            fontSize: 14,
            fontFamily: "'Fira Code','JetBrains Mono','Cascadia Code',Menlo,monospace",
            fontWeight: '400', fontWeightBold: '600',
            letterSpacing: 0, lineHeight: 1.2,
            theme: THEME, allowProposedApi: true,
            scrollback: 10000,
            smoothScrollDuration: 100,
            macOptionIsMeta: true,
            drawBoldTextInBrightColors: true,
            rightClickSelectsWord: true
        });
        var fit = new FitAddon.FitAddon();
        term.loadAddon(fit);
        term.loadAddon(new WebLinksAddon.WebLinksAddon());
        term.open(body);

        setTimeout(function () { fit.fit(); }, 60);

        var shellId = shell.id;
        term.onData(function (data) {
            if (focusedId !== shellId) return;
            encrypt(cryptoKey, new TextEncoder().encode(data)).then(function (enc) {
                wsSend({ t: 'data', tid: shellId, d: enc });
            });
        });

        term.onResize(function (size) {
            wsSend({ t: 'resize', tid: shellId, c: size.cols, r: size.rows });
        });

        term.attachCustomKeyEventHandler(function (e) {
            if ((e.ctrlKey || e.metaKey) && e.key === 'v' && e.type === 'keydown') {
                navigator.clipboard.readText().then(function (text) {
                    if (text && focusedId === shellId) {
                        encrypt(cryptoKey, new TextEncoder().encode(text)).then(function (enc) {
                            wsSend({ t: 'data', tid: shellId, d: enc });
                        });
                    }
                }).catch(function () {});
                return false;
            }
            if ((e.ctrlKey || e.metaKey) && e.key === 'c' && e.type === 'keydown') {
                var sel = term.getSelection();
                if (sel) {
                    navigator.clipboard.writeText(sel).catch(function () {});
                    return false;
                }
            }
            return true;
        });

        body.addEventListener('paste', function (e) {
            e.preventDefault();
            var text = (e.clipboardData || window.clipboardData).getData('text');
            if (text && focusedId === shellId) {
                encrypt(cryptoKey, new TextEncoder().encode(text)).then(function (enc) {
                    wsSend({ t: 'data', tid: shellId, d: enc });
                });
            }
        });

        // Close button handler
        closeBtn.addEventListener('click', function (e) {
            e.preventDefault();
            e.stopPropagation();
            closeShell(shellId);
        });

        // Minimize (collapse to just the header)
        minimizeBtn.addEventListener('click', function (e) {
            e.preventDefault();
            e.stopPropagation();
            el.classList.toggle('minimized');
            if (!el.classList.contains('minimized')) {
                setTimeout(function () { fit.fit(); }, 60);
            }
        });

        // Maximize (fill viewport)
        maximizeBtn.addEventListener('click', function (e) {
            e.preventDefault();
            e.stopPropagation();
            el.classList.toggle('maximized');
            if (el.classList.contains('maximized')) {
                var rect = viewport.getBoundingClientRect();
                el.style.width = (rect.width / zoom - 40) + 'px';
                el.style.height = (rect.height / zoom - 60) + 'px';
            } else {
                el.style.width = '720px';
                el.style.height = '480px';
            }
            setTimeout(function () { fit.fit(); }, 60);
        });

        el.addEventListener('mousedown', function (e) {
            if (e.target.closest('.traffic-lights') || e.target === resizeHandle) return;
            focusShell(shellId);
        });

        setupDrag(header, el, shell);
        setupResize(resizeHandle, el, shell, term, fit);

        shells[shell.id] = {
            id: shell.id, name: shell.name,
            x: shell.x, y: shell.y,
            el: el, term: term, fit: fit,
            header: header, body: body,
            active: shell.active,
            dot: dot
        };

        return shells[shell.id];
    }

    function closeShell(id) {
        var s = shells[id];
        if (!s) return;
        // Send close request to server
        wsSend({ t: 'close', tid: id });
        // Animate removal
        if (s.el) {
            s.el.classList.add('closing');
            setTimeout(function () { removeShell(id); }, 200);
        } else {
            removeShell(id);
        }
    }

    function focusShell(id) {
        if (focusedId === id) return;
        Object.keys(shells).forEach(function (sid) {
            if (shells[sid].el) shells[sid].el.classList.remove('focused');
        });
        focusedId = id;
        var s = shells[id];
        if (s && s.el) {
            s.el.classList.add('focused');
            s.el.style.zIndex = nextZ();
            s.term.focus();
        }
        wsSend({ t: 'focus', tid: id });
    }

    var zCounter = 10;
    function nextZ() { return ++zCounter; }

    function setupDrag(handle, el, shell) {
        var startX, startY, origX, origY, dragging = false;

        handle.addEventListener('mousedown', function (e) {
            if (e.target.closest('.traffic-lights') || e.target.closest('.shell-header-right')) return;
            e.preventDefault();
            e.stopPropagation();
            startX = e.clientX;
            startY = e.clientY;
            origX = shell.x;
            origY = shell.y;
            dragging = true;
            el.classList.add('dragging');
            el.style.zIndex = nextZ();

            function onMove(ev) {
                if (!dragging) return;
                var dx = (ev.clientX - startX) / zoom;
                var dy = (ev.clientY - startY) / zoom;
                shell.x = origX + dx;
                shell.y = origY + dy;
                el.style.transform = 'translate(' + shell.x + 'px,' + shell.y + 'px)';
                if (shells[shell.id]) {
                    shells[shell.id].x = shell.x;
                    shells[shell.id].y = shell.y;
                }
            }
            function onUp() {
                dragging = false;
                el.classList.remove('dragging');
                document.removeEventListener('mousemove', onMove);
                document.removeEventListener('mouseup', onUp);
                wsSend({ t: 'move', tid: shell.id, x: shell.x, y: shell.y });
            }
            document.addEventListener('mousemove', onMove);
            document.addEventListener('mouseup', onUp);
        });
    }

    function setupResize(handle, el, shell, term, fit) {
        handle.addEventListener('mousedown', function (e) {
            e.preventDefault();
            e.stopPropagation();
            var startX = e.clientX, startY = e.clientY;
            var origW = el.offsetWidth, origH = el.offsetHeight;
            el.classList.add('resizing');

            function onMove(ev) {
                var w = Math.max(400, origW + (ev.clientX - startX) / zoom);
                var h = Math.max(200, origH + (ev.clientY - startY) / zoom);
                el.style.width = w + 'px';
                el.style.height = h + 'px';
                fit.fit();
            }
            function onUp() {
                el.classList.remove('resizing');
                document.removeEventListener('mousemove', onMove);
                document.removeEventListener('mouseup', onUp);
                fit.fit();
            }
            document.addEventListener('mousemove', onMove);
            document.addEventListener('mouseup', onUp);
        });
    }

    function showEmptyHint(show) {
        var hint = document.getElementById('empty-hint');
        if (!hint) return;
        hint.style.display = show ? 'flex' : 'none';
    }

    function removeShell(id) {
        var s = shells[id];
        if (!s) return;
        if (s.term) s.term.dispose();
        if (s.el && s.el.parentNode) s.el.parentNode.removeChild(s.el);
        delete shells[id];
        if (focusedId === id) {
            focusedId = null;
            var first = Object.keys(shells)[0];
            if (first) focusShell(first);
        }
        showEmptyHint(Object.keys(shells).length === 0);
    }

    // --- Remote Cursors ---
    var remoteCursors = {};

    function renderCursor(msg) {
        if (msg.u === myUserId) return;
        var el = remoteCursors[msg.u];
        if (!el) {
            el = document.createElement('div');
            el.className = 'remote-cursor';
            el.innerHTML = '<svg width="18" height="22" viewBox="0 0 18 22"><path d="M1 1l14 10-6.5 1.5L5.5 20z" fill="' +
                esc(msg.color || '#58a6ff') + '" stroke="' + esc(msg.color || '#58a6ff') +
                '" stroke-width="1.5" stroke-linejoin="round"/></svg>' +
                '<span class="cursor-name" style="background:' + esc(msg.color || '#58a6ff') + '">' +
                esc(msg.n || '?') + '</span>';
            cursorLayer.appendChild(el);
            remoteCursors[msg.u] = el;
        }
        el.style.transform = 'translate(' + msg.x + 'px,' + msg.y + 'px)';
    }

    function removeCursor(uid) {
        if (remoteCursors[uid]) {
            remoteCursors[uid].remove();
            delete remoteCursors[uid];
        }
    }

    // --- Latency Display ---
    function latencyClass(ms) {
        if (ms <= 80) return 'latency-great';
        if (ms <= 150) return 'latency-good';
        if (ms <= 300) return 'latency-ok';
        return 'latency-bad';
    }

    function updateLatencyDisplay() {
        var el = document.getElementById('my-latency');
        if (!el) return;
        var display = Math.round(smoothedLatency);
        el.textContent = display + 'ms';
        el.className = 'latency-badge ' + latencyClass(display);
        el.title = 'Latency: ' + display + 'ms | Jitter: ' + Math.round(jitter) + 'ms | Raw: ' + currentLatency + 'ms';
    }

    function recordLatency(ms) {
        currentLatency = ms;
        if (smoothedLatency === 0) {
            smoothedLatency = ms;
        } else {
            smoothedLatency = EMA_ALPHA * ms + (1 - EMA_ALPHA) * smoothedLatency;
        }
        latencyHistory.push(ms);
        if (latencyHistory.length > LATENCY_HISTORY_SIZE) latencyHistory.shift();
        if (latencyHistory.length >= 2) {
            var sum = 0;
            for (var i = 1; i < latencyHistory.length; i++) {
                sum += Math.abs(latencyHistory[i] - latencyHistory[i - 1]);
            }
            jitter = sum / (latencyHistory.length - 1);
        }
        updateLatencyDisplay();
    }

    // --- User Avatars ---
    function renderUsers(list) {
        users = {};
        userAvatarsEl.innerHTML = '';
        var count = 0;
        (list || []).forEach(function (u) {
            users[u.id] = u;
            count++;
            var av = document.createElement('div');
            av.className = 'user-avatar';
            av.style.background = u.color;
            var latStr = u.latency > 0 ? ' (' + u.latency + 'ms)' : '';
            av.textContent = (u.name || '?')[0].toUpperCase();
            av.title = u.name + latStr;
            userAvatarsEl.appendChild(av);
        });
        viewersEl.textContent = count + ' user' + (count !== 1 ? 's' : '');

        Object.keys(remoteCursors).forEach(function (uid) {
            if (!users[uid]) removeCursor(uid);
        });
    }

    // --- Canvas Pan & Zoom ---
    viewport.addEventListener('wheel', function (e) {
        e.preventDefault();
        if (e.ctrlKey || e.metaKey) {
            var rect = viewport.getBoundingClientRect();
            var mx = e.clientX - rect.left - rect.width / 2;
            var my = e.clientY - rect.top - rect.height / 2;
            var oldZoom = zoom;
            var delta = -e.deltaY * 0.002;
            zoom = Math.max(MIN_ZOOM, Math.min(MAX_ZOOM, zoom * (1 + delta)));
            var scale = zoom / oldZoom;
            panX = mx / zoom - (mx / oldZoom - panX);
            panY = my / zoom - (my / oldZoom - panY);
            updateTransform();
        } else {
            panX -= e.deltaX / zoom;
            panY -= e.deltaY / zoom;
            updateTransform();
        }
    }, { passive: false });

    var isSelectingText = false;

    viewport.addEventListener('mousedown', function (e) {
        if (e.target.closest('.shell')) return;
        if (e.button === 1 || e.button === 0) {
            isPanning = true;
            panStartX = e.clientX;
            panStartY = e.clientY;
            panStartPanX = panX;
            panStartPanY = panY;
            viewport.classList.add('panning');
            e.preventDefault();
        }
    });

    document.addEventListener('mousedown', function (e) {
        if (e.target.closest('.shell-body') || e.target.closest('.xterm')) {
            isSelectingText = true;
        }
    });

    document.addEventListener('mousemove', function (e) {
        if (isPanning) {
            panX = panStartPanX + (e.clientX - panStartX) / zoom;
            panY = panStartPanY + (e.clientY - panStartY) / zoom;
            updateTransform();
            return;
        }
        if (isSelectingText) return;
        var now = Date.now();
        if (now - lastCursorSend > CURSOR_INTERVAL) {
            lastCursorSend = now;
            var pos = screenToCanvas(e.clientX, e.clientY);
            wsSend({ t: 'cursor', x: Math.round(pos.x), y: Math.round(pos.y) });
        }
    });

    document.addEventListener('mouseup', function () {
        if (isPanning) {
            isPanning = false;
            viewport.classList.remove('panning');
        }
        isSelectingText = false;
    });

    // Touch support
    var touchStartDist = 0, touchStartZoom = 1;
    viewport.addEventListener('touchstart', function (e) {
        if (e.touches.length === 2) {
            var dx = e.touches[0].clientX - e.touches[1].clientX;
            var dy = e.touches[0].clientY - e.touches[1].clientY;
            touchStartDist = Math.sqrt(dx * dx + dy * dy);
            touchStartZoom = zoom;
        } else if (e.touches.length === 1 && (e.target === viewport || e.target === grid)) {
            isPanning = true;
            panStartX = e.touches[0].clientX;
            panStartY = e.touches[0].clientY;
            panStartPanX = panX;
            panStartPanY = panY;
        }
    }, { passive: true });

    viewport.addEventListener('touchmove', function (e) {
        if (e.touches.length === 2 && touchStartDist > 0) {
            var dx = e.touches[0].clientX - e.touches[1].clientX;
            var dy = e.touches[0].clientY - e.touches[1].clientY;
            var dist = Math.sqrt(dx * dx + dy * dy);
            zoom = Math.max(MIN_ZOOM, Math.min(MAX_ZOOM, touchStartZoom * (dist / touchStartDist)));
            updateTransform();
            e.preventDefault();
        } else if (isPanning && e.touches.length === 1) {
            panX = panStartPanX + (e.touches[0].clientX - panStartX) / zoom;
            panY = panStartPanY + (e.touches[0].clientY - panStartY) / zoom;
            updateTransform();
            e.preventDefault();
        }
    }, { passive: false });

    viewport.addEventListener('touchend', function () {
        isPanning = false;
        touchStartDist = 0;
    });

    // Keyboard shortcuts
    document.addEventListener('keydown', function (e) {
        if (e.ctrlKey && e.shiftKey && e.key === 'N') {
            e.preventDefault();
            createNewShell();
        }
        if (e.ctrlKey && e.key === '0') {
            e.preventDefault();
            fitToShells();
        }
    });

    // --- WebSocket ---
    var retryCount = 0;
    var MAX_RETRIES = 999;
    var connectTimer = null;
    var pongWatchdog = null;
    var lastPongTime = 0;
    var PONG_TIMEOUT = 60000;
    var wsConnected = false;
    var lastConnectTime = 0;

    var bootTimer = null;
    function showOverlay(text, sub) {
        overlayEl.style.display = 'flex';
        var sp = document.querySelector('.spinner');
        if (sp) sp.style.display = '';
        document.getElementById('overlay-text').textContent = text;
        var subEl = document.getElementById('overlay-sub');
        if (subEl) subEl.innerHTML = sub || '';
    }

    function hideOverlay() {
        overlayEl.style.display = 'none';
        if (bootTimer) { clearInterval(bootTimer); bootTimer = null; }
    }

    function animateBoot() {
        var lines = [
            '> Initializing secure channel...',
            '> Negotiating AES-256-GCM encryption...',
            '> Key exchange complete',
            '> Establishing WebSocket tunnel...',
            '> Session authenticated',
            '> Terminal ready'
        ];
        var subEl = document.getElementById('overlay-sub');
        if (!subEl) return;
        subEl.innerHTML = '';
        var idx = 0;
        bootTimer = setInterval(function () {
            if (idx >= lines.length) { clearInterval(bootTimer); bootTimer = null; return; }
            var span = document.createElement('div');
            span.className = 'boot-line';
            span.textContent = lines[idx];
            subEl.appendChild(span);
            idx++;
        }, 350);
    }

    function retryDelay() {
        if (retryCount <= 2) return 100;
        if (retryCount <= 5) return 500;
        if (retryCount <= 10) return 1000;
        if (retryCount <= 20) return 2000;
        return 5000;
    }

    function sendPing() {
        if (ws && ws.readyState === 1) {
            pendingPingTs = Date.now();
            pendingPingPerf = performance.now();
            wsSend({ t: 'ping', ts: pendingPingTs });
        }
    }

    function startPongWatchdog() {
        if (pongWatchdog) clearInterval(pongWatchdog);
        lastPongTime = Date.now();
        pongWatchdog = setInterval(function () {
            if (Date.now() - lastPongTime > PONG_TIMEOUT && ws) {
                console.warn('[minisocketx] pong timeout, reconnecting');
                ws.close();
            }
        }, 5000);
    }

    function stopTimers() {
        if (pingInterval) { clearInterval(pingInterval); pingInterval = null; }
        if (pongWatchdog) { clearInterval(pongWatchdog); pongWatchdog = null; }
        if (connectTimer) { clearTimeout(connectTimer); connectTimer = null; }
        if (bootTimer) { clearInterval(bootTimer); bootTimer = null; }
    }

    function connect() {
        stopTimers();
        if (ws) {
            try { ws.onclose = null; ws.onerror = null; ws.close(); } catch (e) {}
            ws = null;
        }

        if (retryCount > 0 && !wsConnected) {
            showOverlay('Connecting to session...', retryCount > 2 ?
                '<span style="color:#94a3b8;font-size:0.8rem">Attempt ' + (retryCount + 1) + ' — checking connection...</span>' : '');
        }

        var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
        var url = proto + '//' + location.host + '/ws/view/' + SESSION_ID + '?name=' + encodeURIComponent(userName);

        try {
            ws = new WebSocket(url);
        } catch (e) {
            setStatus('Connection failed', 'disconnected');
            showOverlay('Connection failed', '<span style="color:#94a3b8;font-size:0.8rem">' + (e.message || 'WebSocket error') + '</span>');
            scheduleRetry();
            return;
        }

        connectTimer = setTimeout(function () {
            if (ws && ws.readyState === 0) {
                console.warn('[minisocketx] connection timeout');
                ws.close();
            }
        }, 15000);

        ws.onopen = function () {
            if (connectTimer) { clearTimeout(connectTimer); connectTimer = null; }
            wsConnected = true;
            lastConnectTime = Date.now();
            retryCount = 0;
            setStatus('Connected', 'connected');
            hideOverlay();
            smoothedLatency = 0;
            latencyHistory = [];
            pingInterval = setInterval(sendPing, PING_INTERVAL);
            sendPing();
            startPongWatchdog();
        };

        ws.onclose = function (ev) {
            stopTimers();
            var wasConnected = wsConnected;
            wsConnected = false;
            if (wasConnected && Date.now() - lastConnectTime > 5000) {
                retryCount = 0;
            }
            if (retryCount < 5) {
                setStatus('Reconnecting...', 'connected');
            } else {
                setStatus('Reconnecting...', 'disconnected');
            }
            if (retryCount >= 8) {
                showOverlay('Reconnecting...', '<span style="color:#94a3b8;font-size:0.8rem">Connection unstable — retrying (' + (retryCount + 1) + ')</span>');
            }
            scheduleRetry();
        };

        ws.onerror = function () {
            if (!wsConnected) {
                setStatus('Connection error', 'disconnected');
            }
        };

        ws.onmessage = function (ev) {
            try { handleMessage(JSON.parse(ev.data)); } catch (e) {}
        };
    }

    window.__msxRetry = function() { retryCount = 0; connect(); };

    function scheduleRetry() {
        if (retryCount >= MAX_RETRIES) {
            setStatus('Disconnected', 'disconnected');
            showOverlay('Unable to connect',
                '<span style="color:#94a3b8;font-size:0.8rem">Session may have ended.</span><br>' +
                '<a href="/" style="color:#60a5fa;text-decoration:none;font-size:0.85rem;margin-top:8px;display:inline-block">Back to Home</a>' +
                '<br><button onclick="window.__msxRetry()" style="margin-top:8px;background:#1e293b;border:1px solid #334155;color:#e2e8f0;padding:6px 16px;border-radius:6px;cursor:pointer;font-size:0.85rem">Try Again</button>');
            var sp = document.querySelector('.spinner');
            if (sp) sp.style.display = 'none';
            return;
        }
        retryCount++;
        var delay = retryDelay();
        setTimeout(function () {
            if (!ws || ws.readyState >= 2) connect();
        }, delay);
    }

    function handleMessage(msg) {
        switch (msg.t) {
            case 'pong':
                lastPongTime = Date.now();
                if (msg.ts && pendingPingTs === msg.ts) {
                    var rtt = Math.round(performance.now() - pendingPingPerf);
                    recordLatency(rtt);
                    wsSend({ t: 'latency', latency: rtt });
                }
                break;

            case 'identity':
                myUserId = msg.u;
                myColor = msg.color;
                break;

            case 'shells':
                var incoming = msg.shells || [];
                var incomingIds = {};
                incoming.forEach(function (s) {
                    incomingIds[s.id] = true;
                    if (shells[s.id]) {
                        shells[s.id].active = s.active;
                        shells[s.id].name = s.name;
                        if (shells[s.id].dot) {
                            shells[s.id].dot.className = 'shell-dot' + (s.active ? ' on' : '');
                        }
                        if (shells[s.id].header) {
                            var titleEl = shells[s.id].header.querySelector('.shell-title');
                            if (titleEl) titleEl.textContent = s.name || s.id;
                        }
                    } else {
                        createShellElement(s);
                    }
                });
                Object.keys(shells).forEach(function (id) {
                    if (!incomingIds[id]) removeShell(id);
                });
                if (!focusedId && incoming.length > 0) {
                    focusShell(incoming[0].id);
                    fitToShells();
                }
                showEmptyHint(incoming.length === 0 && Object.keys(shells).length === 0);
                break;

            case 'data':
                if (msg.tid && shells[msg.tid] && msg.d) {
                    decrypt(cryptoKey, msg.d).then(function (pt) {
                        shells[msg.tid].term.write(pt);
                    }).catch(function () {});
                }
                break;

            case 'resize':
                if (msg.tid && shells[msg.tid] && msg.c > 0 && msg.r > 0) {
                    shells[msg.tid].term.resize(msg.c, msg.r);
                }
                break;

            case 'users':
                renderUsers(msg.users || []);
                break;

            case 'cursor':
                renderCursor(msg);
                break;

            case 'move':
                if (msg.tid && shells[msg.tid]) {
                    shells[msg.tid].x = msg.x;
                    shells[msg.tid].y = msg.y;
                    shells[msg.tid].el.style.transform = 'translate(' + msg.x + 'px,' + msg.y + 'px)';
                }
                break;

            case 'created':
                if (msg.tid && !shells[msg.tid]) {
                    createShellElement({ id: msg.tid, name: msg.n, x: msg.x, y: msg.y, active: false });
                    focusShell(msg.tid);
                }
                break;

            case 'closed':
                if (msg.tid) removeShell(msg.tid);
                break;
        }
    }

    function createNewShell() {
        var name = prompt('Terminal name:');
        if (name === null) return;
        if (!name) name = '';
        wsSend({ t: 'create', n: name });
    }

    // --- Init ---
    newShellBtn.addEventListener('click', createNewShell);
    zoomFitBtn.addEventListener('click', fitToShells);

    function init() {
        importKey(secret).then(function (k) {
            cryptoKey = k;
            var controller = typeof AbortController !== 'undefined' ? new AbortController() : null;
            var fetchTimeout = setTimeout(function () {
                if (controller) controller.abort();
            }, 10000);
            var opts = controller ? { signal: controller.signal } : {};
            return fetch('/api/sessions/' + SESSION_ID, opts).finally(function () {
                clearTimeout(fetchTimeout);
            });
        }).then(function (r) {
            if (!r || !r.ok) {
                var sp = document.querySelector('.spinner');
                if (sp) sp.style.display = 'none';
                document.getElementById('overlay-text').textContent = 'Session not found or expired.';
                var sub = document.getElementById('overlay-sub');
                if (sub) sub.innerHTML = '<a href="/" style="color:#60a5fa;text-decoration:none;font-family:inherit;font-size:0.85rem">Back to Home</a>';
                return null;
            }
            return r.json();
        }).then(function (info) {
            if (!info) return;

            hideOverlay();
            nameModal.style.display = 'flex';
            nameInput.focus();

            function start() {
                userName = nameInput.value.trim() || 'Anonymous';
                nameModal.style.display = 'none';
                showOverlay('Connecting to session...', '');
                animateBoot();
                connect();
            }

            nameBtn.onclick = start;
            nameInput.onkeydown = function (e) { if (e.key === 'Enter') start(); };
        }).catch(function (err) {
            console.error('[minisocketx] init error:', err);
            var sp = document.querySelector('.spinner');
            if (sp) sp.style.display = 'none';
            var msg = 'Failed to connect.';
            if (err && err.name === 'AbortError') msg = 'Connection timed out.';
            document.getElementById('overlay-text').textContent = msg;
            var sub = document.getElementById('overlay-sub');
            if (sub) sub.innerHTML = '<span style="color:#94a3b8;font-size:0.8rem">' +
                (err && err.message && err.name !== 'AbortError' ? err.message : 'Could not reach the server') + '</span>' +
                '<br><button onclick="location.reload()" style="margin-top:12px;background:#1e293b;border:1px solid #334155;color:#e2e8f0;padding:6px 16px;border-radius:6px;cursor:pointer;font-size:0.85rem">Retry</button>';
        });
    }

    var resizeTimer;
    window.addEventListener('resize', function () {
        clearTimeout(resizeTimer);
        resizeTimer = setTimeout(function () {
            Object.keys(shells).forEach(function (id) {
                if (shells[id].fit) shells[id].fit.fit();
            });
        }, 150);
    });

    updateTransform();

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', init);
    } else {
        init();
    }
})();
