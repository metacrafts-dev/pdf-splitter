package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"pdf-splitter/cleaner"
)

// handleEvents maintains a lightweight Server-Sent Events stream for standalone desktop
// tabs so background tab throttling and F5 reloads do not prematurely kill the local server.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Standalone {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Disable per-request write deadline for the long-lived SSE connection.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	s.activeSSE.Add(1)
	s.everConnected.Store(true)
	defer s.activeSSE.Add(-1)

	_, _ = fmt.Fprint(w, "event: ready\ndata: ok\n\n")
	flusher.Flush()

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// runStandaloneWatchdog monitors active SSE clients (and legacy heartbeats) in standalone mode.
// Once all browser tabs disconnect and remain disconnected for 15 seconds (surviving F5 reloads
// and background tab throttling), it purges local sessions and triggers graceful shutdown.
func (s *Server) runStandaloneWatchdog(ctx context.Context) {
	// Initial grace period for browser launch and first tab connection.
	select {
	case <-ctx.Done():
		return
	case <-time.After(20 * time.Second):
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var zeroSince time.Time
	const disconnectGrace = 15 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			connected := s.activeSSE.Load() > 0
			if !connected && s.heartbeatStarted.Load() {
				last := time.Unix(s.lastHeartbeat.Load(), 0)
				if time.Since(last) <= disconnectGrace {
					connected = true
				}
			}

			if connected {
				zeroSince = time.Time{}
				continue
			}

			if !s.everConnected.Load() && !s.heartbeatStarted.Load() {
				continue
			}

			if zeroSince.IsZero() {
				zeroSince = time.Now()
				continue
			}

			if time.Since(zeroSince) >= disconnectGrace {
				if s.shutdownTriggered.CompareAndSwap(false, true) {
					slog.Info("all standalone browser tabs closed; purging local sessions and shutting down")
					_ = cleaner.PurgeAllSessions(s.cfg.SessionsDir)
					if s.cancelServer != nil {
						s.cancelServer()
					}
					return
				}
			}
		}
	}
}

// handleHeartbeat supports explicit standalone keepalive pings.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Standalone {
		http.NotFound(w, r)
		return
	}
	s.lastHeartbeat.Store(time.Now().Unix())
	s.heartbeatStarted.Store(true)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     "ok",
		"standalone": s.cfg.Standalone,
	})
}

// handleShutdown explicitly stops the standalone desktop server and cleans session data.
// Only registered and allowed in Standalone mode.
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Standalone {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, controlBodyLimit)
	sessionID := extractSessionIDFromRequest(r)

	if sessionID != "" {
		if sessionDir, err := s.safeSessionDir(sessionID); err == nil {
			s.evictSession(sessionID)
			_ = os.RemoveAll(sessionDir)
			if s.s3Configured() {
				s.bgWG.Add(1)
				go func(sID string) {
					defer s.bgWG.Done()
					ctxTimeout, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer cancel()
					_, _ = s.store.DeletePrefix(ctxTimeout, fmt.Sprintf("sessions/%s/", sID))
				}(sessionID)
			}
		}
	}

	if s.shutdownTriggered.CompareAndSwap(false, true) {
		slog.Info("standalone shutdown requested; purging local sessions")
		go func() {
			time.Sleep(150 * time.Millisecond)
			_ = cleaner.PurgeAllSessions(s.cfg.SessionsDir)
			if s.cancelServer != nil {
				s.cancelServer()
			}
		}()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     "ok",
		"standalone": s.cfg.Standalone,
	})
}

// handleOpenFolder opens the validated session parts folder in the local OS file manager.
// Only available in local Standalone non-headless mode; accepts session_id (never raw paths).
func (s *Server) handleOpenFolder(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Standalone || s.cfg.Headless {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, controlBodyLimit)
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	sessionDir, err := s.safeSessionDir(strings.TrimSpace(req.SessionID))
	if err != nil {
		jsonError(w, "Valid session_id required", http.StatusBadRequest)
		return
	}

	partsDir, err := ensureContainedPath(sessionDir, filepath.Join(sessionDir, "parts"))
	if err != nil {
		jsonError(w, "Invalid output directory", http.StatusBadRequest)
		return
	}

	fi, err := os.Stat(partsDir)
	if err != nil || !fi.IsDir() {
		jsonError(w, "Output folder not found", http.StatusNotFound)
		return
	}

	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" || os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
		go openExplorer(partsDir)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func openBrowser(rawURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	case "darwin":
		cmd = exec.Command("open", rawURL)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		cmd = exec.Command("xdg-open", rawURL)
	}
	_ = cmd.Start()
}

func openExplorer(folderPath string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer.exe", folderPath)
	case "darwin":
		cmd = exec.Command("open", folderPath)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		cmd = exec.Command("xdg-open", folderPath)
	}
	_ = cmd.Start()
}
