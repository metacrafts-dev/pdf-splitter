package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	controlBodyLimit int64 = 64 << 10 // 64 KiB max body for JSON/form control endpoints
	multipartMemMax  int64 = 32 << 20 // 32 MiB max in-RAM buffer for multipart form parsing
)

// statusTrackingWriter records whether response headers/body have been written so
// recoveryMiddleware can safely emit a 500 JSON error only when no response started yet.
type statusTrackingWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *statusTrackingWriter) WriteHeader(code int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusTrackingWriter) Write(p []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(p)
}

func (w *statusTrackingWriter) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (w *statusTrackingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// recoveryMiddleware catches any panic in downstream handlers, logs the stack trace,
// and returns a structured 500 JSON error instead of dropping the TCP connection (which
// causes reverse proxies and Cloudflare Tunnels to return an opaque 502).
func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &statusTrackingWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered in HTTP handler",
					"panic", rec,
					"method", r.Method,
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
				if !tw.wroteHeader {
					jsonError(tw, "Internal server error", http.StatusInternalServerError)
				}
			}
		}()
		next.ServeHTTP(tw, r)
	})
}

func securityHeadersMiddleware(next http.Handler) http.Handler {
	const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self' https: http:; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}

// requireCSRF enforces same-origin Fetch Metadata and per-boot CSRF token verification
// on state-changing HTTP requests.
func (s *Server) requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site != "" {
			if site != "same-origin" && site != "none" {
				jsonError(w, "Cross-origin request rejected", http.StatusForbidden)
				return
			}
		}

		token := strings.TrimSpace(r.Header.Get("X-CSRF-Token"))
		if token == "" {
			token = strings.TrimSpace(r.URL.Query().Get("csrf_token"))
		}

		if subtle.ConstantTimeCompare([]byte(token), []byte(s.csrfToken)) != 1 {
			jsonError(w, "Invalid or missing CSRF token", http.StatusForbidden)
			return
		}

		next(w, r)
	}
}

func (s *Server) rateLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := extractClientIP(r)
		if !s.limiter.Allow(ip) {
			w.Header().Set("Retry-After", "5")
			jsonError(w, "Too many requests; please try again shortly", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// extractClientIP trusts proxy headers only when the immediate TCP peer is loopback.
func extractClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	parsed := net.ParseIP(strings.Trim(host, "[]"))
	if parsed != nil && parsed.IsLoopback() {
		if cfIP := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cfIP != "" {
			return cfIP
		}
		if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
			return realIP
		}
		if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
			if first, _, ok := strings.Cut(xff, ","); ok {
				return strings.TrimSpace(first)
			}
			return xff
		}
	}
	return host
}

// isValidSessionID strictly requires a 32-character lowercase hexadecimal ID.
func isValidSessionID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// safeSessionDir resolves <SessionsDir>/<sessionID> and verifies strict containment.
func (s *Server) safeSessionDir(sessionID string) (string, error) {
	if !isValidSessionID(sessionID) {
		return "", fmt.Errorf("invalid session ID")
	}
	target := filepath.Join(s.cfg.SessionsDir, sessionID)
	return ensureContainedPath(s.cfg.SessionsDir, target)
}

// ensureContainedPath verifies target stays strictly inside baseDir.
func ensureContainedPath(baseDir, target string) (string, error) {
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absBase, absTarget)
	if err != nil {
		return "", err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path escapes base directory")
	}
	return absTarget, nil
}

// sanitizeFilename strips directory paths, control characters, quotes, and path-unsafe symbols
// while preserving Unicode/Arabic letters for user-friendly download names.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(strings.TrimSpace(name))

	var b strings.Builder
	for _, r := range name {
		if unicode.IsControl(r) || r == '/' || r == '\\' || r == '"' || r == '\'' ||
			r == ';' || r == ':' || r == '*' || r == '?' || r == '<' || r == '>' || r == '|' {
			continue
		}
		b.WriteRune(r)
	}

	cleaned := strings.TrimSpace(b.String())
	cleaned = strings.TrimLeft(cleaned, ".")
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "book.pdf"
	}
	if len(cleaned) > 180 {
		ext := filepath.Ext(cleaned)
		if len(ext) > 10 {
			ext = ""
		}
		cleaned = cleaned[:180-len(ext)] + ext
	}
	return cleaned
}

// ipRateLimiter is a lightweight, zero-dependency per-IP token bucket limiter.
type ipRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rate    float64
	burst   float64
}

type tokenBucket struct {
	tokens   float64
	lastSeen time.Time
}

func newIPRateLimiter(ratePerSec, burst float64) *ipRateLimiter {
	return &ipRateLimiter{
		buckets: make(map[string]*tokenBucket),
		rate:    ratePerSec,
		burst:   burst,
	}
}

func (l *ipRateLimiter) Allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[ip]
	if !ok {
		l.buckets[ip] = &tokenBucket{
			tokens:   l.burst - 1,
			lastSeen: now,
		}
		return true
	}

	elapsed := now.Sub(b.lastSeen).Seconds()
	b.lastSeen = now
	b.tokens += elapsed * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *ipRateLimiter) StartCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cutoff := time.Now().Add(-15 * time.Minute)
				l.mu.Lock()
				for ip, b := range l.buckets {
					if b.lastSeen.Before(cutoff) {
						delete(l.buckets, ip)
					}
				}
				l.mu.Unlock()
			}
		}
	}()
}
