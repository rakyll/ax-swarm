// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

//go:embed traceui
var traceUIFiles embed.FS

const (
	sessionCookie   = "axo_trace_session"
	loginTokenTTL   = 2 * time.Minute
	sessionTTL      = 12 * time.Hour
	maxSSEClients   = 8
	rateLimitPerSec = 50
	rateLimitBurst  = 100
)

// TraceServer serves the live trace viewer for one run on 127.0.0.1.
//
// Access control: the server binds to loopback only and checks the Host
// header (blocking DNS-rebinding pages). Opening the viewer mints a random
// one-time login token that is exchanged for an HttpOnly, SameSite=Strict
// session cookie, then redirected away so it does not linger in the URL bar.
// Every other request requires that cookie.
type TraceServer struct {
	tracer *Tracer
	ln     net.Listener
	srv    *http.Server
	host   string // "127.0.0.1:<port>"

	mu       sync.Mutex
	logins   map[string]time.Time // one-time login token -> expiry
	sessions map[string]time.Time // session id -> expiry
	clients  int

	limiter *rateLimiter
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// StartTraceServer listens on a random loopback port and serves the viewer.
func StartTraceServer(t *Tracer) (*TraceServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("trace viewer: %w", err)
	}
	s := &TraceServer{
		tracer:   t,
		ln:       ln,
		host:     ln.Addr().String(),
		logins:   make(map[string]time.Time),
		sessions: make(map[string]time.Time),
		limiter:  newRateLimiter(rateLimitPerSec, rateLimitBurst),
	}
	s.srv = &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// Close stops the server.
func (s *TraceServer) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

// LoginURL mints a one-time login URL for the viewer.
func (s *TraceServer) LoginURL() (string, error) {
	tok, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	now := time.Now()
	for k, exp := range s.logins {
		if now.After(exp) {
			delete(s.logins, k)
		}
	}
	s.logins[tok] = now.Add(loginTokenTTL)
	s.mu.Unlock()
	return "http://" + s.host + "/login?token=" + tok, nil
}

// Open mints a login URL and opens it in the default browser, returning the URL.
func (s *TraceServer) Open() (string, error) {
	u, err := s.LoginURL()
	if err != nil {
		return "", err
	}
	return u, openBrowser(u)
}

// openBrowser opens url with the platform's fixed opener binary. url is always
// a loopback URL built by LoginURL, never user input.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func setSecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), payment=()")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	// No Access-Control-Allow-* headers: cross-origin reads are not allowed.
}

func (s *TraceServer) handler() http.Handler {
	static, _ := fs.Sub(traceUIFiles, "traceui")
	mux := http.NewServeMux()
	mux.HandleFunc("/login", s.handleLogin)
	mux.Handle("/", s.requireSession(http.FileServer(http.FS(static))))
	mux.Handle("/events", s.requireSession(http.HandlerFunc(s.handleEvents)))
	mux.Handle("/trace.json", s.requireSession(http.HandlerFunc(s.handleJSON)))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header())
		// DNS rebinding guard: only the exact loopback address we serve on.
		if r.Host != s.host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.limiter.allow() {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *TraceServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	s.mu.Lock()
	var ok bool
	now := time.Now()
	for k, exp := range s.logins {
		if subtle.ConstantTimeCompare([]byte(k), []byte(tok)) == 1 && now.Before(exp) {
			ok = true
			delete(s.logins, k) // one-time use
			break
		}
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, "this viewer link has expired; press ctrl+t in axo for a new one", http.StatusForbidden)
		return
	}
	sid, err := randomToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.sessions[sid] = now.Add(sessionTTL)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sid,
		Path:     "/",
		MaxAge:   int(sessionTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// TODO(security): Secure is not set because the viewer is served over
		// plain HTTP on 127.0.0.1 (loopback traffic never leaves the host);
		// the cookie is still HttpOnly, SameSite=Strict, and host-only.
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *TraceServer) validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, k)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(k), []byte(c.Value)) == 1 {
			return true
		}
	}
	return false
}

func (s *TraceServer) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.validSession(r) {
			http.Error(w, "unauthorized; press ctrl+t in axo to open the trace viewer", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *TraceServer) handleJSON(w http.ResponseWriter, r *http.Request) {
	snap, _, cancel := s.tracer.Subscribe()
	cancel()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(snap)
}

// handleEvents streams the trace as Server-Sent Events: one snapshot, then
// one message per span change.
func (s *TraceServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	if s.clients >= maxSSEClients {
		s.mu.Unlock()
		http.Error(w, "too many viewers", http.StatusTooManyRequests)
		return
	}
	s.clients++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.clients--
		s.mu.Unlock()
	}()

	// SSE streams outlive the server's ReadTimeout-derived write deadline.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	snap, updates, cancel := s.tracer.Subscribe()
	defer cancel()
	if err := writeSSE(w, snap); err != nil {
		return
	}
	flusher.Flush()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-updates:
			if !ok {
				return // fell behind; the browser reconnects for a new snapshot
			}
			if err := writeSSE(w, msg); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, data []byte) error {
	// JSON from encoding/json never contains raw newlines, so one data line suffices.
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	return nil
}

// rateLimiter is a token bucket shared by all requests.
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newRateLimiter(perSec, burst int) *rateLimiter {
	return &rateLimiter{rate: float64(perSec), burst: float64(burst), tokens: float64(burst), last: time.Now()}
}

func (l *rateLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.tokens = min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}
