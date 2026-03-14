/*
 * Copyright (c) 2025 Risqi Nur Fadhilah
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in
 * all copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING
 * FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER
 * DEALINGS IN THE SOFTWARE.
 *
 * ----------------------------------------------------------------------------
 * Project      : GO-TUNNEL PRO
 * Developers   : Risqi Nur Fadhilah
 * Tester       : Rerechan02
 * Version      : v1.3-Stable
 * License      : MIT License
 * ----------------------------------------------------------------------------
 *
 * CMD Compile:
 * CGO_ENABLED=0 go build -ldflags "-s -w \
 *   -X 'main.Credits=Risqi Nur Fadhilah' \
 *   -X 'main.Version=v1.3-Stable'" -o ssh-ws
 *
 * Requirements:
 * - Debian 11 / Ubuntu 22.04+
 * - Go version 1.22.0 or higher
 * - Dropbear or OpenSSH server
 * - Access to /var/log/auth.log
 * - Nginx (optional, for CDN/reverse-proxy front-end)
 */

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mukswilly/udpgw"
)

// ─── Build-time variables ────────────────────────────────────────────────────

var (
	Version = "v1.3-Stable"
	Credits = "Risqi Nur Fadhilah"
)

// ─── ANSI color codes ────────────────────────────────────────────────────────

const (
	ColorReset  = "\033[0m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorCyan   = "\033[36m"
	ColorGray   = "\033[90m"
	ColorPurple = "\033[35m"
	ColorBlue   = "\033[34m"
	ColorWhite  = "\033[97m"
	ColorBold   = "\033[1m"
)

// ─── Tunnel / WebSocket constants ────────────────────────────────────────────

const (
	// RFC 6455 §1.3 – magic GUID for Sec-WebSocket-Accept derivation
	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	// idleTimeout: disconnect only after 1000 hours of absolute silence.
	// SSH keepalives will reset this on every packet, so real sessions are
	// effectively permanent while the SSH client is alive.
	idleTimeout = 1000 * time.Hour

	// initialReadTimeout: time allowed for the client to send its HTTP payload.
	initialReadTimeout = 15 * time.Second

	// dialTimeout: time allowed to reach the SSH backend.
	dialTimeout = 10 * time.Second

	// copyBuf: per-goroutine I/O buffer size.
	copyBuf = 32 * 1024
)

// ─── Config ──────────────────────────────────────────────────────────────────

type Config struct {
	BindAddr    string
	Port        int
	Password    string
	FallbackAddr string
	LogFile     string
	AuthLogPath string
	APIPort     int
}

// ─── I/O helpers ─────────────────────────────────────────────────────────────

// WriteCounter wraps an io.Writer and atomically counts bytes written.
type WriteCounter struct {
	Writer  io.Writer
	Counter *int64
}

func (wc WriteCounter) Write(p []byte) (int, error) {
	n, err := wc.Writer.Write(p)
	if n > 0 {
		atomic.AddInt64(wc.Counter, int64(n))
	}
	return n, err
}

// ─── Session types ───────────────────────────────────────────────────────────

type SessionInfo struct {
	ID             string    `json:"id"`
	RealClientIP   string    `json:"real_client_ip"`
	RealClientPort string    `json:"real_client_port"`
	ClientAddr     string    `json:"client_addr"`
	ClientPort     string    `json:"client_port"`
	TargetAddr     string    `json:"target_addr"`
	TargetPort     string    `json:"target_port"`
	ProxyToSSHPort string    `json:"proxy_to_ssh_port"`
	Username       string    `json:"username"`
	SessionNumber  int       `json:"session_number"`
	PID            int       `json:"pid"`
	SSHType        string    `json:"ssh_type"`
	StartTime      time.Time `json:"start_time"`
	LastActivity   time.Time `json:"last_activity"`
	TxBytes        int64     `json:"tx_bytes"`
	RxBytes        int64     `json:"rx_bytes"`
	Duration       string    `json:"duration"`
	TxFormatted    string    `json:"tx_formatted"`
	RxFormatted    string    `json:"rx_formatted"`
	TotalFormatted string    `json:"total_formatted"`
}

type APIResponse struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

type SessionsResponse struct {
	TotalSessions  int64                `json:"total_sessions"`
	ActiveSessions int                  `json:"active_sessions"`
	ClosedSessions int64                `json:"closed_sessions"`
	Sessions       []SessionInfo        `json:"sessions"`
	UserStats      map[string]UserStats `json:"user_stats"`
}

type UserStats struct {
	Username       string `json:"username"`
	SessionCount   int    `json:"session_count"`
	TotalTX        int64  `json:"total_tx"`
	TotalRX        int64  `json:"total_rx"`
	TotalBytes     int64  `json:"total_bytes"`
	TxFormatted    string `json:"tx_formatted"`
	RxFormatted    string `json:"rx_formatted"`
	TotalFormatted string `json:"total_formatted"`
}

// ─── Global state ────────────────────────────────────────────────────────────

var (
	activeSessions   sync.Map
	sessionCounter   int64
	sshPortToSession sync.Map
	serverStartTime  time.Time
)

// ═════════════════════════════════════════════════════════════════════════════
// main
// ═════════════════════════════════════════════════════════════════════════════

func main() {
	cfg := setupFlags()
	setupLogger(cfg.LogFile)
	serverStartTime = time.Now()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	printProxyBanner()

	if cfg.APIPort > 0 {
		go startAPIServer(cfg.APIPort)
	}

	go sessionMonitor(ctx)
	go authLogMonitor(ctx, cfg.AuthLogPath)

	// UDPGW multiplexer
	go func() {
		configJSON := fmt.Sprintf(`{
			"LogLevel": "info",
			"LogFilename": "%s",
			"HostID": "proxy-server",
			"UdpgwPort": 7300,
			"DNSResolverIPAddress": "1.1.1.1"
		}`, cfg.LogFile)

		logInfo("UDPGW", "Initializing Multiplexer on port 7300...")
		if err := udpgw.StartServer([]byte(configJSON)); err != nil {
			logError("UDPGW", fmt.Sprintf("Service Error: %v", err))
		}
	}()

	serverAddr := fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.Port)
	listener, err := net.Listen("tcp", serverAddr)
	if err != nil {
		logError("CORE", fmt.Sprintf("Failed to bind %s: %v", serverAddr, err))
		os.Exit(1)
	}

	go func() {
		logInfo("SSHWS", fmt.Sprintf("Listening on %s%s%s", ColorCyan, serverAddr, ColorReset))
		logInfo("AUTH", fmt.Sprintf("Monitoring: %s%s%s", ColorCyan, cfg.AuthLogPath, ColorReset))
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					continue
				}
			}
			go handleConnection(conn, cfg)
		}
	}()

	<-ctx.Done()
	fmt.Println("\n" + ColorYellow + " [!] Shutdown signal received. Closing all connections..." + ColorReset)
	logSessionSummary()
	listener.Close()
	time.Sleep(1 * time.Second)
	logInfo("SYSTEM", "Server halted successfully.")
}

// ═════════════════════════════════════════════════════════════════════════════
// WebSocket helpers
// ═════════════════════════════════════════════════════════════════════════════

// buildWSHandshake constructs a valid RFC 6455 HTTP/1.1 101 response.
//
// It scans the *entire* raw payload (including split/custom payloads that
// contain more than one HTTP request block) to locate:
//   - Sec-WebSocket-Key  → used to compute Sec-WebSocket-Accept
//   - Sec-WebSocket-Protocol → echoed back if present
//
// If no key is found (plain-HTTP or non-WS client) a minimal 101 is returned
// so the tunnel still works.
func buildWSHandshake(rawPayload string) string {
	key := getHeaderFromPayload(rawPayload, "Sec-WebSocket-Key")

	if key == "" {
		// Fallback – no WS key present (direct TCP / custom payload without key)
		return "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n\r\n"
	}

	// SHA1(key + GUID) → base64  (RFC 6455 §4.2.2)
	h := sha1.New()
	h.Write([]byte(strings.TrimSpace(key) + wsGUID))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	var sb strings.Builder
	sb.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	sb.WriteString("Upgrade: websocket\r\n")
	sb.WriteString("Connection: Upgrade\r\n")
	sb.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n")

	// Echo first sub-protocol if offered
	if proto := getHeaderFromPayload(rawPayload, "Sec-WebSocket-Protocol"); proto != "" {
		first := strings.TrimSpace(strings.SplitN(proto, ",", 2)[0])
		sb.WriteString("Sec-WebSocket-Protocol: " + first + "\r\n")
	}

	sb.WriteString("\r\n")
	return sb.String()
}

// ═════════════════════════════════════════════════════════════════════════════
// Connection handling
// ═════════════════════════════════════════════════════════════════════════════

func handleConnection(clientConn net.Conn, cfg Config) {
	defer clientConn.Close()

	// Apply TCP-level tuning immediately
	setTCPOptions(clientConn)

	sessionID := generateSessionID()
	clientAddr := clientConn.RemoteAddr().String()
	clientIP, clientPort := splitHostPort(clientAddr)

	// ── Read initial HTTP payload ─────────────────────────────────────────
	// 8 KiB is enough for even the most verbose custom/enhanced payloads.
	clientConn.SetReadDeadline(time.Now().Add(initialReadTimeout))
	buf := make([]byte, 8192)
	n, err := clientConn.Read(buf)
	if err != nil {
		return
	}
	clientConn.SetReadDeadline(time.Time{}) // clear – idle handled by doTransfer
	rawPayload := string(buf[:n])

	// ── Resolve real client IP (behind CDN / Nginx) ───────────────────────
	realClientIP   := clientIP
	realClientPort := clientPort

	if xfwd := getHeaderFromPayload(rawPayload, "X-Forwarded-For"); xfwd != "" {
		realClientIP = strings.TrimSpace(strings.SplitN(xfwd, ",", 2)[0])
	} else if xrip := getHeaderFromPayload(rawPayload, "X-Real-IP"); xrip != "" {
		realClientIP = xrip
	}

	// ── Target resolution ─────────────────────────────────────────────────
	targetHost := getHeaderFromPayload(rawPayload, "X-Real-Host")
	if targetHost == "" {
		targetHost = cfg.FallbackAddr
	}

	// ── Optional password auth ────────────────────────────────────────────
	authPass := getHeaderFromPayload(rawPayload, "X-Pass")
	if cfg.Password != "" && authPass != cfg.Password {
		logWarn("AUTH", fmt.Sprintf("[%s] Unauthorized from %s:%s", sessionID, realClientIP, realClientPort))
		clientConn.Write([]byte("HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n"))
		return
	}

	if !strings.Contains(targetHost, ":") {
		targetHost += ":22"
	}
	_, targetPort := splitHostPort(targetHost)

	// ── Dial SSH backend ──────────────────────────────────────────────────
	targetConn, err := net.DialTimeout("tcp", targetHost, dialTimeout)
	if err != nil {
		logError("TUNNEL", fmt.Sprintf("[%s] Failed to reach %s from %s:%s – %v",
			sessionID, targetHost, realClientIP, realClientPort, err))
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	defer targetConn.Close()

	setTCPOptions(targetConn)

	proxyLocalAddr := targetConn.LocalAddr().String()
	_, proxyToSSHPort := splitHostPort(proxyLocalAddr)

	// ── Register session ──────────────────────────────────────────────────
	session := &SessionInfo{
		ID:             sessionID,
		RealClientIP:   realClientIP,
		RealClientPort: realClientPort,
		ClientAddr:     clientIP,
		ClientPort:     clientPort,
		TargetAddr:     targetHost,
		TargetPort:     targetPort,
		ProxyToSSHPort: proxyToSSHPort,
		Username:       "detecting...",
		SSHType:        "unknown",
		StartTime:      time.Now(),
		LastActivity:   time.Now(),
	}
	activeSessions.Store(sessionID, session)
	sshPortToSession.Store(proxyToSSHPort, sessionID)

	logSuccess("CONNECT", fmt.Sprintf("[%s] %s:%s → %s  (proxy-port:%s)",
		sessionID, realClientIP, realClientPort, targetHost, proxyToSSHPort))

	// ── WebSocket 101 handshake ───────────────────────────────────────────
	clientConn.Write([]byte(buildWSHandshake(rawPayload)))

	// ── Bidirectional pipe ────────────────────────────────────────────────
	doTransfer(clientConn, targetConn, session)
	sshPortToSession.Delete(proxyToSSHPort)
}

// doTransfer pipes data between client↔SSH with:
//   - 1000-hour idle timeout (refreshed on every chunk of data)
//   - per-direction byte counters
//   - graceful half-close on EOF
//   - clean shutdown on any side's error
func doTransfer(client, target net.Conn, session *SessionInfo) {
	var wg sync.WaitGroup
	wg.Add(2)

	stopMonitor := make(chan struct{})

	// ── Background monitor ────────────────────────────────────────────────
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopMonitor:
				return
			case <-ticker.C:
				tx := atomic.LoadInt64(&session.TxBytes)
				rx := atomic.LoadInt64(&session.RxBytes)
				if tx > 0 || rx > 0 {
					session.LastActivity = time.Now()
					dur := time.Since(session.StartTime).Round(time.Second)
					ui := formatUserDisplay(session.Username, session.SessionNumber, session.PID)
					log.Printf("%s[MONITOR]%s [%s] %s:%s | %s | up:%v | TX:%s RX:%s Total:%s",
						ColorPurple, ColorReset,
						session.ID, session.RealClientIP, session.RealClientPort,
						ui, dur,
						formatBytes(tx), formatBytes(rx), formatBytes(tx+rx))
				}
			}
		}
	}()

	// refreshDeadline pushes the idle deadline forward on both ends.
	refreshDeadline := func() {
		d := time.Now().Add(idleTimeout)
		client.SetDeadline(d)
		target.SetDeadline(d)
	}

	// Set initial deadline
	refreshDeadline()

	// ── copyHalf pipes src→dst, counting bytes ────────────────────────────
	copyHalf := func(dst, src net.Conn, counter *int64) {
		defer wg.Done()
		buf := make([]byte, copyBuf)
		for {
			nr, rerr := src.Read(buf)
			if nr > 0 {
				nw, werr := dst.Write(buf[:nr])
				if nw > 0 {
					atomic.AddInt64(counter, int64(nw))
					refreshDeadline() // activity → reset idle timer
				}
				if werr != nil {
					if !isConnClosed(werr) {
						logWarn("TUNNEL", fmt.Sprintf("[%s] write error: %v", session.ID, werr))
					}
					break
				}
			}
			if rerr != nil {
				if rerr != io.EOF && !isConnClosed(rerr) {
					logWarn("TUNNEL", fmt.Sprintf("[%s] read error: %v", session.ID, rerr))
				}
				break
			}
		}
		// Graceful half-close: signal EOF to the other side
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			dst.Close()
		}
	}

	go copyHalf(target, client, &session.TxBytes) // client → SSH
	go copyHalf(client, target, &session.RxBytes) // SSH → client

	wg.Wait()
	close(stopMonitor)

	// ── Session end log ───────────────────────────────────────────────────
	dur   := time.Since(session.StartTime).Round(time.Second)
	tx    := atomic.LoadInt64(&session.TxBytes)
	rx    := atomic.LoadInt64(&session.RxBytes)
	ui    := formatUserDisplay(session.Username, session.SessionNumber, session.PID)

	log.Printf("%s[END]%s [%s] %s:%s | User: %s%s%s | up:%v | TX:%s RX:%s Total:%s",
		ColorGray, ColorReset,
		session.ID, session.RealClientIP, session.RealClientPort,
		ColorBold, ui, ColorReset,
		dur, formatBytes(tx), formatBytes(rx), formatBytes(tx+rx))

	activeSessions.Delete(session.ID)
}

// isConnClosed returns true for expected/benign network errors so we don't
// spam the log on normal connection teardown.
func isConnClosed(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return err == io.EOF ||
		strings.Contains(s, "use of closed network connection") ||
		strings.Contains(s, "connection reset by peer") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "EOF") ||
		strings.Contains(s, "i/o timeout")
}

// setTCPOptions enables keepalive and disables Nagle on a TCP connection.
// Both are essential for long-lived SSH tunnels:
//   - KeepAlive: OS sends probes so dead peers are detected without application
//     data, preventing half-open connections from persisting forever.
//   - NoDelay: eliminates 40 ms Nagle delay on small SSH packets (interactive
//     typing feels instant).
func setTCPOptions(conn net.Conn) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(60 * time.Second) // probe every 60 s
	tc.SetNoDelay(true)
}

// ═════════════════════════════════════════════════════════════════════════════
// Header parsing (works on split / enhanced / custom payloads)
// ═════════════════════════════════════════════════════════════════════════════

// getHeaderFromPayload scans EVERY line of the raw payload regardless of
// which HTTP request block it belongs to.  This handles custom injection
// formats like:
//
//	GET /cdn-cgi/trace HTTP/1.1\r\nHost: x\r\n\r\n
//	CF-RAY / HTTP/1.1\r\nHost: y\r\nUpgrade: websocket\r\nSec-WebSocket-Key: …\r\n\r\n
func getHeaderFromPayload(payload, key string) string {
	needle := strings.ToLower(key) + ":"
	for _, raw := range strings.Split(payload, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.HasPrefix(strings.ToLower(line), needle) {
			return strings.TrimSpace(line[len(needle):])
		}
	}
	return ""
}

// getHeader is kept for backward compatibility – it wraps getHeaderFromPayload.
func getHeader(headers, key string) string {
	return getHeaderFromPayload(headers, key)
}

// ═════════════════════════════════════════════════════════════════════════════
// HTTP API server
// ═════════════════════════════════════════════════════════════════════════════

func startAPIServer(port int) {
	mux := http.NewServeMux()

	cors := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Content-Type", "application/json")
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			next(w, r)
		}
	}

	mux.HandleFunc("/api/status", cors(handleStatus))
	mux.HandleFunc("/api/sessions", cors(handleSessions))
	mux.HandleFunc("/api/sessions/active", cors(handleActiveSessions))
	mux.HandleFunc("/api/users", cors(handleUsers))
	mux.HandleFunc("/api/stats", cors(handleStats))
	mux.HandleFunc("/health", cors(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(APIResponse{Success: true, Message: "OK"})
	}))

	addr := fmt.Sprintf(":%d", port)
	logInfo("API", fmt.Sprintf("HTTP API listening on %s%s%s", ColorCyan, addr, ColorReset))

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logError("API", fmt.Sprintf("Server error: %v", err))
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	active := 0
	activeSessions.Range(func(_, _ interface{}) bool { active++; return true })
	total := atomic.LoadInt64(&sessionCounter)
	up := time.Since(serverStartTime)
	json.NewEncoder(w).Encode(APIResponse{Success: true, Data: map[string]interface{}{
		"version":         Version,
		"uptime":          up.String(),
		"uptime_seconds":  int(up.Seconds()),
		"total_sessions":  total,
		"active_sessions": active,
		"closed_sessions": total - int64(active),
	}})
}

func handleSessions(w http.ResponseWriter, r *http.Request) {
	var sessions []SessionInfo
	userStats := make(map[string]UserStats)

	activeSessions.Range(func(_, v interface{}) bool {
		s := v.(*SessionInfo)
		tx := atomic.LoadInt64(&s.TxBytes)
		rx := atomic.LoadInt64(&s.RxBytes)
		sessions = append(sessions, SessionInfo{
			ID:             s.ID,
			RealClientIP:   s.RealClientIP,
			RealClientPort: s.RealClientPort,
			Username:       s.Username,
			SessionNumber:  s.SessionNumber,
			PID:            s.PID,
			SSHType:        s.SSHType,
			StartTime:      s.StartTime,
			TxBytes:        tx,
			RxBytes:        rx,
			Duration:       time.Since(s.StartTime).Round(time.Second).String(),
			TxFormatted:    formatBytes(tx),
			RxFormatted:    formatBytes(rx),
			TotalFormatted: formatBytes(tx + rx),
		})
		if s.Username != "detecting..." && s.Username != "" {
			us := userStats[s.Username]
			us.Username = s.Username
			us.SessionCount++
			us.TotalTX += tx
			us.TotalRX += rx
			us.TotalBytes = us.TotalTX + us.TotalRX
			us.TxFormatted = formatBytes(us.TotalTX)
			us.RxFormatted = formatBytes(us.TotalRX)
			us.TotalFormatted = formatBytes(us.TotalBytes)
			userStats[s.Username] = us
		}
		return true
	})

	total := atomic.LoadInt64(&sessionCounter)
	json.NewEncoder(w).Encode(APIResponse{Success: true, Data: SessionsResponse{
		TotalSessions:  total,
		ActiveSessions: len(sessions),
		ClosedSessions: total - int64(len(sessions)),
		Sessions:       sessions,
		UserStats:      userStats,
	}})
}

func handleActiveSessions(w http.ResponseWriter, r *http.Request) {
	var sessions []SessionInfo
	activeSessions.Range(func(_, v interface{}) bool {
		s := v.(*SessionInfo)
		tx := atomic.LoadInt64(&s.TxBytes)
		rx := atomic.LoadInt64(&s.RxBytes)
		sessions = append(sessions, SessionInfo{
			ID:             s.ID,
			RealClientIP:   s.RealClientIP,
			Username:       s.Username,
			SessionNumber:  s.SessionNumber,
			PID:            s.PID,
			SSHType:        s.SSHType,
			Duration:       time.Since(s.StartTime).Round(time.Second).String(),
			TxFormatted:    formatBytes(tx),
			RxFormatted:    formatBytes(rx),
			TotalFormatted: formatBytes(tx + rx),
		})
		return true
	})
	json.NewEncoder(w).Encode(APIResponse{Success: true, Data: map[string]interface{}{
		"count": len(sessions), "sessions": sessions,
	}})
}

func handleUsers(w http.ResponseWriter, r *http.Request) {
	userStats := make(map[string]UserStats)
	activeSessions.Range(func(_, v interface{}) bool {
		s := v.(*SessionInfo)
		if s.Username == "detecting..." || s.Username == "" {
			return true
		}
		tx := atomic.LoadInt64(&s.TxBytes)
		rx := atomic.LoadInt64(&s.RxBytes)
		us := userStats[s.Username]
		us.Username = s.Username
		us.SessionCount++
		us.TotalTX += tx
		us.TotalRX += rx
		us.TotalBytes = us.TotalTX + us.TotalRX
		us.TxFormatted = formatBytes(us.TotalTX)
		us.RxFormatted = formatBytes(us.TotalRX)
		us.TotalFormatted = formatBytes(us.TotalBytes)
		userStats[s.Username] = us
		return true
	})
	var ul []UserStats
	for _, u := range userStats {
		ul = append(ul, u)
	}
	json.NewEncoder(w).Encode(APIResponse{Success: true, Data: map[string]interface{}{
		"count": len(ul), "users": ul,
	}})
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	var tx, rx int64
	active := 0
	users := make(map[string]int)
	activeSessions.Range(func(_, v interface{}) bool {
		active++
		s := v.(*SessionInfo)
		tx += atomic.LoadInt64(&s.TxBytes)
		rx += atomic.LoadInt64(&s.RxBytes)
		if s.Username != "detecting..." && s.Username != "" {
			users[s.Username]++
		}
		return true
	})
	total := atomic.LoadInt64(&sessionCounter)
	up := time.Since(serverStartTime)
	json.NewEncoder(w).Encode(APIResponse{Success: true, Data: map[string]interface{}{
		"uptime":          up.String(),
		"uptime_seconds":  int(up.Seconds()),
		"total_sessions":  total,
		"active_sessions": active,
		"closed_sessions": total - int64(active),
		"total_tx":        tx,
		"total_rx":        rx,
		"total_bytes":     tx + rx,
		"tx_formatted":    formatBytes(tx),
		"rx_formatted":    formatBytes(rx),
		"total_formatted": formatBytes(tx + rx),
		"unique_users":    len(users),
		"users_breakdown": users,
	}})
}

// ═════════════════════════════════════════════════════════════════════════════
// Auth-log monitor (Dropbear + OpenSSH username detection)
// ═════════════════════════════════════════════════════════════════════════════

func authLogMonitor(ctx context.Context, authLogPath string) {
	dropbearRe := regexp.MustCompile(
		`^[A-Z][a-z]{2}\s+\d{1,2}\s+\d{2}:\d{2}:\d{2}\s+` +
			`\S+\s+dropbear\[(\d+)\]:\s+` +
			`Password auth succeeded for '([^']{1,32})'\s+` +
			`from\s+((?:\d{1,3}\.){3}\d{1,3}):(\d{1,5})`,
	)
	opensshRe := regexp.MustCompile(
		`^[A-Z][a-z]{2}\s+\d{1,2}\s+\d{2}:\d{2}:\d{2}\s+` +
			`\S+\s+sshd\[(\d+)\]:\s+` +
			`Accepted (?:password|publickey|keyboard-interactive)\s+` +
			`for\s+([a-zA-Z0-9_-]{1,32})\s+` +
			`from\s+((?:\d{1,3}\.){3}\d{1,3})\s+` +
			`port\s+(\d{1,5})\s+ssh2?$`,
	)

	file, err := os.Open(authLogPath)
	if err != nil {
		logWarn("AUTH", fmt.Sprintf("Cannot open %s: %v (username detection disabled)", authLogPath, err))
		return
	}
	defer file.Close()

	file.Seek(0, io.SeekEnd)
	reader := bufio.NewReader(file)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					break
				}
				if m := dropbearRe.FindStringSubmatch(line); len(m) == 5 {
					if isStrictValidDropbearMatch(m[1], m[2], m[3], m[4]) {
						pid, _ := strconv.Atoi(m[1])
						updateSessionUsername(pid, m[2], m[4], "dropbear")
					}
				}
				if m := opensshRe.FindStringSubmatch(line); len(m) == 5 {
					if isStrictValidOpenSSHMatch(m[1], m[2], m[3], m[4]) {
						pid, _ := strconv.Atoi(m[1])
						updateSessionUsername(pid, m[2], m[4], "openssh")
					}
				}
			}
		}
	}
}

func isStrictValidDropbearMatch(pidStr, username, ipAddr, portStr string) bool {
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid < 1 || pid > 9999999 {
		return false
	}
	if len(username) < 1 || len(username) > 32 {
		return false
	}
	for _, c := range username {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	if net.ParseIP(ipAddr) == nil {
		return false
	}
	port, err := strconv.Atoi(portStr)
	return err == nil && port >= 1 && port <= 65535
}

func isStrictValidOpenSSHMatch(pidStr, username, ipAddr, portStr string) bool {
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid < 1 || pid > 9999999 {
		return false
	}
	if len(username) < 1 || len(username) > 32 {
		return false
	}
	if !((username[0] >= 'a' && username[0] <= 'z') ||
		(username[0] >= 'A' && username[0] <= 'Z') ||
		username[0] == '_') {
		return false
	}
	for _, c := range username {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	if net.ParseIP(ipAddr) == nil {
		return false
	}
	port, err := strconv.Atoi(portStr)
	return err == nil && port >= 1 && port <= 65535
}

func updateSessionUsername(pid int, username, sshPort, sshType string) {
	sid, ok := sshPortToSession.Load(sshPort)
	if !ok {
		return
	}
	val, exists := activeSessions.Load(sid)
	if !exists {
		return
	}
	s := val.(*SessionInfo)
	s.Username = username
	s.PID = pid
	s.SSHType = sshType
	if s.SessionNumber == 0 {
		s.SessionNumber = getNextSessionNumber(username)
	}
	activeSessions.Store(sid, s)

	tag := ""
	switch sshType {
	case "dropbear":
		tag = " [Dropbear]"
	case "openssh":
		tag = " [OpenSSH]"
	}
	logInfo("AUTH", fmt.Sprintf("[%s] Authenticated: %s%s%s%s",
		s.ID, ColorBold, formatUserDisplay(username, s.SessionNumber, pid), ColorReset, tag))
}

// ═════════════════════════════════════════════════════════════════════════════
// Periodic monitors
// ═════════════════════════════════════════════════════════════════════════════

func sessionMonitor(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			count := 0
			var users []string
			activeSessions.Range(func(_, v interface{}) bool {
				count++
				s := v.(*SessionInfo)
				if s.Username != "detecting..." && s.Username != "" && s.SessionNumber > 0 {
					users = append(users, fmt.Sprintf("%s-%d", s.Username, s.SessionNumber))
				}
				return true
			})
			if count > 0 {
				label := strings.Join(users, ", ")
				if label == "" {
					label = "authenticating..."
				}
				logInfo("MONITOR", fmt.Sprintf("Active sessions: %s%d%s [%s]",
					ColorBold, count, ColorReset, label))
			}
		}
	}
}

func logSessionSummary() {
	sep := ColorCyan + strings.Repeat("═", 57) + ColorReset
	fmt.Println("\n" + sep)
	fmt.Println(ColorCyan + "                 SESSION SUMMARY" + ColorReset)
	fmt.Println(sep)

	total := atomic.LoadInt64(&sessionCounter)
	active := 0
	type urow struct{ count int; tx, rx int64 }
	umap := make(map[string]*urow)

	activeSessions.Range(func(_, v interface{}) bool {
		active++
		s := v.(*SessionInfo)
		tx := atomic.LoadInt64(&s.TxBytes)
		rx := atomic.LoadInt64(&s.RxBytes)
		fmt.Printf("  [%s] %s:%s | %s | up:%v | TX:%s RX:%s\n",
			s.ID, s.RealClientIP, s.RealClientPort,
			formatUserDisplay(s.Username, s.SessionNumber, s.PID),
			time.Since(s.StartTime).Round(time.Second),
			formatBytes(tx), formatBytes(rx))
		if s.Username != "detecting..." && s.Username != "" {
			if umap[s.Username] == nil {
				umap[s.Username] = &urow{}
			}
			umap[s.Username].count++
			umap[s.Username].tx += tx
			umap[s.Username].rx += rx
		}
		return true
	})

	if len(umap) > 0 {
		fmt.Println("\n  Per-User Statistics:")
		for u, r := range umap {
			fmt.Printf("    %-20s %d sessions | TX:%s RX:%s Total:%s\n",
				u+":", r.count, formatBytes(r.tx), formatBytes(r.rx), formatBytes(r.tx+r.rx))
		}
	}

	fmt.Printf("\n  Total:%d  Active:%d  Closed:%d\n", total, active, total-int64(active))
	fmt.Println(sep)
}

// ═════════════════════════════════════════════════════════════════════════════
// Misc helpers
// ═════════════════════════════════════════════════════════════════════════════

func getNextSessionNumber(username string) int {
	if username == "" || username == "detecting..." {
		return 0
	}
	max := 0
	activeSessions.Range(func(_, v interface{}) bool {
		if s := v.(*SessionInfo); s.Username == username && s.SessionNumber > max {
			max = s.SessionNumber
		}
		return true
	})
	return max + 1
}

func formatUserDisplay(username string, num, pid int) string {
	s := username
	if username != "detecting..." && username != "" && num > 0 {
		s = fmt.Sprintf("%s-%d", username, num)
	}
	if pid > 0 {
		s = fmt.Sprintf("%s (PID:%d)", s, pid)
	}
	return s
}

func splitHostPort(addr string) (host, port string) {
	i := strings.LastIndex(addr, ":")
	if i == -1 {
		return addr, ""
	}
	return addr[:i], addr[i+1:]
}

func generateSessionID() string {
	n := atomic.AddInt64(&sessionCounter, 1)
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("%04d-%s", n, hex.EncodeToString(b)[:6])
}

func formatBytes(b int64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func setupFlags() Config {
	c := Config{}
	var lf1, lf2, lf3 string
	flag.StringVar(&c.BindAddr, "b", "0.0.0.0", "Bind address")
	flag.IntVar(&c.Port, "p", 2080, "Listen port")
	flag.StringVar(&c.Password, "a", "", "Auth password (optional)")
	flag.StringVar(&c.FallbackAddr, "t", "127.0.0.1:22", "Default SSH target")
	flag.StringVar(&c.AuthLogPath, "auth-log", "/var/log/auth.log", "Auth log path")
	flag.IntVar(&c.APIPort, "api-port", 8081, "HTTP API port (0 = disabled)")
	flag.StringVar(&lf1, "l", "", "Log file")
	flag.StringVar(&lf2, "log", "", "Log file")
	flag.StringVar(&lf3, "logs", "", "Log file")
	flag.Parse()
	if lf1 != "" {
		c.LogFile = lf1
	} else if lf2 != "" {
		c.LogFile = lf2
	} else {
		c.LogFile = lf3
	}
	return c
}

func setupLogger(path string) {
	writers := []io.Writer{os.Stdout}
	if path != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0755)
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
			writers = append(writers, f)
		}
	}
	log.SetOutput(io.MultiWriter(writers...))
	log.SetFlags(log.Ldate | log.Ltime)
}

func printProxyBanner() {
	fmt.Print(ColorCyan)
	fmt.Println(`╔════════════════════════════════════════════════════════╗`)
	fmt.Println(`║    ___  ___  ____  _  ____  __                         `)
	fmt.Println(`║   / _ \/ _ \/ __ \| |/ /\ \/ /                         `)
	fmt.Println(`║  / ___/ , _/ /_/ /  |   \  /                           `)
	fmt.Println(`║ /_/  /_/|_|\____/_/|_|   /_/                           `)
	fmt.Println(`║                                                        `)
	fmt.Printf("║  %s  VERSION   : %-37s %s\n", ColorGray, Version, ColorCyan)
	fmt.Printf("║  %s  DEVELOPER : %-37s %s\n", ColorGray, Credits, ColorCyan)
	fmt.Println(`╚════════════════════════════════════════════════════════╝`)
	fmt.Print(ColorReset)
}

func logInfo(tag, m string)    { log.Printf("%s[%s]%s %s", ColorBlue, tag, ColorReset, m) }
func logSuccess(tag, m string) { log.Printf("%s[%s]%s %s", ColorGreen, tag, ColorReset, m) }
func logWarn(tag, m string)    { log.Printf("%s[%s]%s %s", ColorYellow, tag, ColorReset, m) }
func logError(tag, m string)   { log.Printf("%s[%s]%s %s", ColorRed, tag, ColorReset, m) }
