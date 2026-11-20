// Package server provides the hardened HTTP server for PDF Splitter.
// It serves the embedded web UI, handles local and direct S3 presigned uploads,
// executes concurrency-bounded PDF splitting, builds Store-mode ZIP archives,
// and manages both local disk and S3 lifecycle cleanup.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pdf-splitter/cleaner"
	"pdf-splitter/splitter"
	"pdf-splitter/storage"
)

//go:embed assets/*
var assetsFS embed.FS

// ServerConfig holds the configuration parameters for the HTTP server.
type ServerConfig struct {
	Host           string        // e.g. "127.0.0.1" or "0.0.0.0"
	Port           int           // e.g. 8080
	Retention      time.Duration // Session retention before auto-purge (default: 15m)
	MaxUploadBytes int64         // Max PDF upload size in bytes (default: 500 MiB)
	SessionsDir    string        // Root directory for storing session files
	Headless       bool          // If true, skip launching desktop browser
	Standalone     bool          // If true, desktop standalone mode with SSE auto-shutdown
}

type sessionRecord struct {
	SessionID     string
	Book          *splitter.BookInfo
	ExpectedS3Key string
	ExpectedSize  int64
	FileName      string
	CreatedAt     time.Time
}

// Server encapsulates all HTTP server state, handlers, security controls, and dependencies.
type Server struct {
	cfg           ServerConfig
	store         *storage.Client
	csrfToken     string
	renderedIndex []byte
	mux           *http.ServeMux
	limiter       *ipRateLimiter
	splitSem      chan struct{}

	sessionsMu sync.RWMutex
	sessions   map[string]*sessionRecord

	// Standalone desktop lifecycle tracking (SSE + heartbeat fallback)
	activeSSE         atomic.Int64
	everConnected     atomic.Bool
	lastHeartbeat     atomic.Int64
	heartbeatStarted  atomic.Bool
	shutdownTriggered atomic.Bool
	cancelServer      context.CancelFunc

	bgWG sync.WaitGroup
}

func generateRandomHex(numBytes int) (string, error) {
	b := make([]byte, numBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func generateSessionID() (string, error) {
	return generateRandomHex(16) // 32 lowercase hex chars
}

// NewServer constructs an isolated Server instance with validated config and routes.
func NewServer(cfg ServerConfig, store *storage.Client) (*Server, error) {
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 15 * time.Minute
	}
	if cfg.MaxUploadBytes <= 0 {
		cfg.MaxUploadBytes = 500 << 20
	}
	if cfg.SessionsDir == "" {
		if envSessions := strings.TrimSpace(os.Getenv("SESSIONS_DIR")); envSessions != "" {
			cfg.SessionsDir = envSessions
		} else {
			cwd, err := os.Getwd()
			if err != nil {
				return nil, fmt.Errorf("failed to resolve working directory: %w", err)
			}
			cfg.SessionsDir = filepath.Join(cwd, "dist", "data", "sessions")
		}
	}

	absSessions, err := filepath.Abs(cfg.SessionsDir)
	if err != nil {
		return nil, fmt.Errorf("invalid sessions directory %s: %w", cfg.SessionsDir, err)
	}
	cfg.SessionsDir = absSessions

	if err := os.MkdirAll(cfg.SessionsDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create sessions directory %s: %w", cfg.SessionsDir, err)
	}

	csrfToken, err := generateRandomHex(32)
	if err != nil {
		return nil, fmt.Errorf("failed to generate CSRF token: %w", err)
	}

	rawIndex, err := fs.ReadFile(assetsFS, "assets/index.html")
	if err != nil {
		return nil, fmt.Errorf("failed to load embedded index.html: %w", err)
	}
	renderedIndex := bytes.ReplaceAll(rawIndex, []byte("__CSRF_TOKEN__"), []byte(csrfToken))

	maxConcurrentSplits := runtime.NumCPU()
	if maxConcurrentSplits > 4 {
		maxConcurrentSplits = 4
	}
	if maxConcurrentSplits < 1 {
		maxConcurrentSplits = 1
	}

	s := &Server{
		cfg:           cfg,
		store:         store,
		csrfToken:     csrfToken,
		renderedIndex: renderedIndex,
		mux:           http.NewServeMux(),
		limiter:       newIPRateLimiter(1.0, 30),
		splitSem:      make(chan struct{}, maxConcurrentSplits),
		sessions:      make(map[string]*sessionRecord),
	}

	s.registerRoutes()
	return s, nil
}

// CSRFToken returns the server's per-boot CSRF token (primarily for testing).
func (s *Server) CSRFToken() string {
	return s.csrfToken
}

// Handler returns the root HTTP handler with security headers applied.
func (s *Server) Handler() http.Handler {
	return securityHeadersMiddleware(s.mux)
}

func (s *Server) s3Configured() bool {
	return s.store != nil && s.store.IsConfigured()
}

func (s *Server) evictSession(sessionID string) {
	s.sessionsMu.Lock()
	delete(s.sessions, sessionID)
	s.sessionsMu.Unlock()
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/assets/", s.handleAssets)
	s.mux.HandleFunc("/favicon.ico", s.handleFavicon)
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/api/config", s.handleConfig)
	s.mux.HandleFunc("/api/s3/presign", s.requireCSRF(s.rateLimit(s.handleS3Presign)))
	s.mux.HandleFunc("/api/inspect", s.requireCSRF(s.rateLimit(s.handleInspect)))
	s.mux.HandleFunc("/api/split", s.requireCSRF(s.rateLimit(s.handleSplit)))
	s.mux.HandleFunc("/api/download", s.handleDownload)
	s.mux.HandleFunc("/api/clear", s.requireCSRF(s.handleClear))
	s.mux.HandleFunc("/api/cleanup-session", s.requireCSRF(s.handleClear))
	s.mux.HandleFunc("/api/download-app", s.handleDownloadApp)

	// Desktop-only lifecycle & explorer endpoints are strictly gated to Standalone mode.
	if s.cfg.Standalone {
		s.mux.HandleFunc("/api/events", s.handleEvents)
		s.mux.HandleFunc("/api/heartbeat", s.requireCSRF(s.handleHeartbeat))
		s.mux.HandleFunc("/api/shutdown", s.requireCSRF(s.handleShutdown))
		if !s.cfg.Headless {
			s.mux.HandleFunc("/api/open-folder", s.requireCSRF(s.handleOpenFolder))
		}
	}
}

// Start launches the HTTP server with the provided configuration and blocks until shutdown.
func Start(ctx context.Context, cfg ServerConfig) error {
	s, err := NewServer(cfg, storage.DefaultClient())
	if err != nil {
		return err
	}

	serverCtx, cancelServer := context.WithCancel(ctx)
	defer cancelServer()
	s.cancelServer = cancelServer

	// Start local disk TTL cleaner with in-memory eviction callback.
	cleaner.StartCleaner(serverCtx, s.cfg.SessionsDir, s.cfg.Retention, 1*time.Minute, s.evictSession)
	s.limiter.StartCleanup(serverCtx)

	// Start startup + periodic S3 stale object purge if S3 is configured.
	if s.s3Configured() {
		go s.runS3PurgeLoop(serverCtx)
	}

	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		if s.cfg.Port == 0 {
			listener, err = net.Listen("tcp", fmt.Sprintf("%s:0", s.cfg.Host))
		}
		if err != nil {
			return fmt.Errorf("failed to bind server to %s: %w", addr, err)
		}
	}

	actualPort := listener.Addr().(*net.TCPAddr).Port

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       10 * time.Minute,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	displayHost := s.cfg.Host
	if displayHost == "0.0.0.0" || displayHost == "::" {
		displayHost = "127.0.0.1"
	}
	serverURL := fmt.Sprintf("http://%s:%d", displayHost, actualPort)

	slog.Info("pdf-splitter server starting",
		"url", serverURL,
		"bind", fmt.Sprintf("%s:%d", s.cfg.Host, actualPort),
		"retention", s.cfg.Retention.String(),
		"max_upload_mb", s.cfg.MaxUploadBytes/(1024*1024),
		"sessions_dir", s.cfg.SessionsDir,
		"s3_enabled", s.s3Configured(),
		"standalone", s.cfg.Standalone,
		"headless", s.cfg.Headless,
	)

	if s.cfg.Standalone {
		go s.runStandaloneWatchdog(serverCtx)
	}

	if !s.cfg.Headless {
		go func() {
			time.Sleep(350 * time.Millisecond)
			openBrowser(serverURL)
		}()
	}

	go func() {
		<-serverCtx.Done()
		slog.Info("shutting down HTTP server gracefully")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("graceful shutdown timed out", "err", err)
		}
	}()

	err = srv.Serve(listener)
	s.bgWG.Wait()

	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) runS3PurgeLoop(ctx context.Context) {
	purgeOnce := func() {
		purgeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		purged, err := s.store.PurgeStaleObjects(purgeCtx, "sessions/", s.cfg.Retention)
		if err != nil {
			slog.Warn("S3 stale object purge failed", "err", err)
		} else if purged > 0 {
			slog.Info("S3 stale object purge completed", "purged", purged)
		}
	}

	// Startup purge after brief settle
	select {
	case <-ctx.Done():
		return
	case <-time.After(100 * time.Millisecond):
		purgeOnce()
	}

	interval := s.cfg.Retention / 2
	if interval < 5*time.Minute {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purgeOnce()
		}
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(s.renderedIndex)
	}
}

func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Never expose raw index.html template containing the CSRF placeholder.
	cleanPath := strings.TrimPrefix(r.URL.Path, "/")
	if cleanPath == "assets/index.html" || cleanPath == "assets" || cleanPath == "assets/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.FileServer(http.FS(assetsFS)).ServeHTTP(w, r)
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" fill="none"><rect width="32" height="32" rx="7" fill="#2563eb"/><path d="M7 8.5C7 7.67 7.67 7 8.5 7H14.5C15.33 7 16 7.67 16 8.5V23.5C16 23 15.33 22.5 14.5 22.5H8.5C7.67 22.5 7 23 7 23.5V8.5Z" fill="#ffffff"/><path d="M25 8.5C25 7.67 24.33 7 23.5 7H17.5C16.67 7 16 7.67 16 8.5V23.5C16 23 16.67 22.5 17.5 22.5H23.5C24.33 22.5 25 23 25 23.5V8.5Z" fill="#e2e8f0"/><path d="M15.2 6.5H16.8V25H15.2V6.5Z" fill="#f59e0b"/><circle cx="16" cy="25" r="1.5" fill="#f59e0b"/></svg>`
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(faviconSVG))
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
