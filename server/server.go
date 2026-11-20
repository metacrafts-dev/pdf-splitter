// Package server provides the HTTP server for the Pdf Splitter application.
// It serves the embedded web GUI, handles PDF uploads (both local and direct S3 presigned),
// executes split operations, packages output parts into ZIP archives, facilitates downloads,
// serves application binaries, and manages automatic file retention and S3 lifecycle cleanup.
package server

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pdf-plitter/cleaner"
	"pdf-plitter/splitter"
	"pdf-plitter/storage"
)

//go:embed assets/index.html
var indexHTML []byte

// ServerConfig holds the configuration parameters for the HTTP server.
type ServerConfig struct {
	Host        string        // e.g. "0.0.0.0" or "127.0.0.1"
	Port        int           // e.g. 8080
	Retention   time.Duration // Session retention before auto-purge (default: 15m)
	SessionsDir string        // Root directory for storing session files
	Headless    bool          // If true, skip launching desktop browser
	Standalone  bool          // If true, launched as standalone desktop app (auto-terminates on browser closure)
}

// Global server state and session mapping
var (
	activeConfig ServerConfig
	sessionLock  sync.RWMutex
	// map[sessionID]sessionData
	sessionBooks = make(map[string]*splitter.BookInfo)

	// Standalone desktop heartbeat & lifecycle management
	lastHeartbeat      atomic.Int64
	heartbeatStarted   atomic.Bool
	shutdownTriggered  atomic.Bool
	serverShutdownFunc func()
)

func generateSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Start launches the HTTP server with the provided configuration.
func Start(ctx context.Context, cfg ServerConfig) error {
	activeConfig = cfg

	serverCtx, cancelServer := context.WithCancel(ctx)
	defer cancelServer()
	serverShutdownFunc = cancelServer

	if activeConfig.Host == "" {
		activeConfig.Host = "0.0.0.0"
	}
	if activeConfig.SessionsDir == "" {
		if envSessions := os.Getenv("SESSIONS_DIR"); envSessions != "" {
			activeConfig.SessionsDir = envSessions
		} else {
			cwd, _ := os.Getwd()
			// Segregate runtime session data and uploads cleanly into dist/data/sessions
			activeConfig.SessionsDir = filepath.Join(cwd, "dist", "data", "sessions")
		}
	}
	if activeConfig.Retention <= 0 {
		activeConfig.Retention = 15 * time.Minute
	}

	// Ensure sessions directory exists
	if err := os.MkdirAll(activeConfig.SessionsDir, 0755); err != nil {
		return fmt.Errorf("failed to create sessions directory %s: %w", activeConfig.SessionsDir, err)
	}

	// Start the automatic TTL file cleaner in the background
	cleaner.StartCleaner(ctx, activeConfig.SessionsDir, activeConfig.Retention, 1*time.Minute)

	// Startup Purge: scan and purge any stale leftover data in both local sessions and S3
	go func() {
		time.Sleep(100 * time.Millisecond)
		if storage.IsConfigured() {
			purgeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			purged, err := storage.DefaultClient().PurgeStaleObjects(purgeCtx, "sessions/", activeConfig.Retention)
			if err != nil {
				log.Printf("[S3] Startup purge warning: %v", err)
			} else if purged > 0 {
				log.Printf("[S3] Startup purge completed: removed %d stale object(s)", purged)
			}
		}
	}()

	addr := fmt.Sprintf("%s:%d", activeConfig.Host, activeConfig.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		// If port 8080 or specified port failed on 0.0.0.0 / 127.0.0.1, try random port if port was 0
		if activeConfig.Port == 0 {
			listener, err = net.Listen("tcp", fmt.Sprintf("%s:0", activeConfig.Host))
		}
		if err != nil {
			return fmt.Errorf("failed to bind server to %s: %w", addr, err)
		}
	}

	actualPort := listener.Addr().(*net.TCPAddr).Port
	mux := http.NewServeMux()

	// Register HTTP routes
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/favicon.ico", handleFavicon)
	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/api/heartbeat", handleHeartbeat)
	mux.HandleFunc("/api/shutdown", handleShutdown)
	mux.HandleFunc("/api/s3/presign", handleS3Presign)
	mux.HandleFunc("/api/inspect", handleInspect)
	mux.HandleFunc("/api/split", handleSplit)
	mux.HandleFunc("/api/download", handleDownload)
	mux.HandleFunc("/api/clear", handleClear)
	mux.HandleFunc("/api/cleanup-session", handleClear) // Alias for session cleanup
	mux.HandleFunc("/api/download-app", handleDownloadApp)
	mux.HandleFunc("/api/open-folder", handleOpenFolder)

	srv := &http.Server{
		Handler:           securityHeadersMiddleware(mux),
		ReadHeaderTimeout: 15 * time.Second,
	}

	displayHost := activeConfig.Host
	if displayHost == "0.0.0.0" {
		displayHost = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s:%d", displayHost, actualPort)

	fmt.Printf("\n=================================================================\n")
	fmt.Printf("   🚀 Pdf Splitter Server Running\n")
	fmt.Printf("   🌐 Address:      %s (Listening on %s:%d)\n", url, activeConfig.Host, actualPort)
	fmt.Printf("   ⏳ Retention:    Auto-delete files after %v\n", activeConfig.Retention)
	fmt.Printf("   📁 Storage:      %s\n", activeConfig.SessionsDir)
	if storage.IsConfigured() {
		s3Cfg := storage.DefaultClient().Config()
		ep := s3Cfg.Endpoint
		if ep == "" {
			ep = "AWS S3 (" + s3Cfg.Region + ")"
		}
		fmt.Printf("   ☁️  S3 Storage:   Enabled (Bucket: %s, Endpoint: %s)\n", s3Cfg.Bucket, ep)
	} else {
		fmt.Printf("   ☁️  S3 Storage:   Disabled (Local session storage)\n")
	}
	if activeConfig.Standalone {
		fmt.Printf("   🖥️  Mode:         Standalone Desktop (Auto-terminates and cleans on browser closure)\n")
	} else {
		fmt.Printf("   🖥️  Mode:         Persistent Service / VPS Server\n")
	}
	if activeConfig.Headless {
		fmt.Printf("   🖥️  Headless:     Enabled (No browser popup)\n")
	} else {
		fmt.Printf("   🖥️  Browser:      Launching default browser...\n")
	}
	fmt.Printf("   Press Ctrl+C to terminate the server.\n")
	fmt.Printf("=================================================================\n\n")

	// In standalone mode, start a heartbeat watchdog that auto-terminates
	// when the browser tab/window is closed or killed
	if activeConfig.Standalone {
		go func() {
			// Initial grace period of 20 seconds for browser launch and initial connection
			time.Sleep(20 * time.Second)
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-serverCtx.Done():
					return
				case <-ticker.C:
					if heartbeatStarted.Load() {
						last := time.Unix(lastHeartbeat.Load(), 0)
						if time.Since(last) > 6*time.Second {
							if shutdownTriggered.CompareAndSwap(false, true) {
								log.Printf("[SERVER] Standalone browser disconnected (no heartbeat for >6s). Purging all data and shutting down...")
								_ = cleaner.PurgeAllSessions(activeConfig.SessionsDir)
								cancelServer()
								return
							}
						}
					}
				}
			}
		}()
	}

	// Attempt to launch desktop browser if not headless
	if !activeConfig.Headless {
		go func() {
			time.Sleep(350 * time.Millisecond)
			openBrowser(url)
		}()
	}

	// Graceful shutdown handling
	go func() {
		<-serverCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	return srv.Serve(listener)
}

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}

func handleFavicon(w http.ResponseWriter, r *http.Request) {
	const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" fill="none"><rect width="32" height="32" rx="7" fill="#2563eb"/><path d="M7 8.5C7 7.67 7.67 7 8.5 7H14.5C15.33 7 16 7.67 16 8.5V23.5C16 23 15.33 22.5 14.5 22.5H8.5C7.67 22.5 7 23 7 23.5V8.5Z" fill="#ffffff"/><path d="M25 8.5C25 7.67 24.33 7 23.5 7H17.5C16.67 7 16 7.67 16 8.5V23.5C16 23 16.67 22.5 17.5 22.5H23.5C24.33 22.5 25 23 25 23.5V8.5Z" fill="#e2e8f0"/><path d="M15.2 6.5H16.8V25H15.2V6.5Z" fill="#f59e0b"/><circle cx="16" cy="25" r="1.5" fill="#f59e0b"/></svg>`
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(faviconSVG))
}

// handleConfig returns the server capabilities, S3 status, and configured retention duration.
func handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s3Configured := storage.IsConfigured()
	s3Cfg := storage.DefaultClient().Config()

	resp := map[string]interface{}{
		"s3_enabled":        s3Configured,
		"s3_bucket":         s3Cfg.Bucket,
		"s3_region":         s3Cfg.Region,
		"retention_minutes": int(activeConfig.Retention.Minutes()),
		"standalone":        activeConfig.Standalone,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleHeartbeat receives periodic keepalive pings from the browser in standalone mode.
func handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	lastHeartbeat.Store(time.Now().Unix())
	heartbeatStarted.Store(true)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "ok",
		"standalone": activeConfig.Standalone,
	})
}

// handleShutdown immediately destroys the server and clears data when in standalone mode.
func handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract session_id if present (optional during shutdown)
	var sessionID string
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var req struct {
			SessionID string `json:"session_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		sessionID = req.SessionID
	} else {
		sessionID = strings.TrimSpace(r.FormValue("session_id"))
		if sessionID == "" {
			sessionID = strings.TrimSpace(r.FormValue("session"))
		}
	}

	sessionID = filepath.Clean(sessionID)
	if sessionID != "" && sessionID != "." && !strings.Contains(sessionID, "/") && !strings.Contains(sessionID, "\\") {
		sessionLock.Lock()
		delete(sessionBooks, sessionID)
		sessionLock.Unlock()

		sessionDir := filepath.Join(activeConfig.SessionsDir, sessionID)
		_ = os.RemoveAll(sessionDir)

		if storage.IsConfigured() {
			go func(sID string) {
				ctxTimeout, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				_, _ = storage.DefaultClient().DeletePrefix(ctxTimeout, fmt.Sprintf("sessions/%s/", sID))
			}(sessionID)
		}
		log.Printf("[SERVER] Cleared session immediately: %s", sessionID)
	}

	// If standalone desktop run, destroy server and purge all remaining session data
	if activeConfig.Standalone {
		if shutdownTriggered.CompareAndSwap(false, true) {
			log.Printf("[SERVER] Standalone browser termination signal received. Purging all local data and shutting down...")
			go func() {
				time.Sleep(150 * time.Millisecond)
				_ = cleaner.PurgeAllSessions(activeConfig.SessionsDir)
				if serverShutdownFunc != nil {
					serverShutdownFunc()
				}
			}()
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "ok",
		"standalone": activeConfig.Standalone,
		"session_id": sessionID,
	})
}

// handleS3Presign generates a presigned S3 PUT URL for direct frontend-to-S3 file uploads.
func handleS3Presign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !storage.IsConfigured() {
		jsonError(w, "S3 storage is not configured", http.StatusBadRequest)
		return
	}

	var filename string
	var contentType string

	contentTypeHeader := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentTypeHeader, "application/json") {
		var req struct {
			FileName    string `json:"filename"`
			Name        string `json:"name"`
			ContentType string `json:"content_type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			filename = req.FileName
			if filename == "" {
				filename = req.Name
			}
			contentType = req.ContentType
		}
	} else {
		_ = r.ParseForm()
		filename = r.FormValue("filename")
		if filename == "" {
			filename = r.FormValue("name")
		}
		contentType = r.FormValue("content_type")
	}

	if filename == "" {
		filename = "book.pdf"
	}

	cleanName := filepath.Base(filename)
	sessionID := generateSessionID()
	key := fmt.Sprintf("sessions/%s/input/%s", sessionID, cleanName)

	uploadURL, err := storage.DefaultClient().GeneratePresignedPutURL(r.Context(), key, 45*time.Minute)
	if err != nil {
		jsonError(w, "Failed to generate presigned upload URL: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := map[string]interface{}{
		"success":      true,
		"session_id":   sessionID,
		"key":          key,
		"filename":     cleanName,
		"upload_url":   uploadURL,
		"url":          uploadURL,
		"content_type": contentType,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func handleInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	contentType := r.Header.Get("Content-Type")

	var sessionID string
	var s3Key string
	var fileName string

	// Handle JSON payload (e.g. after direct S3 upload completion)
	if strings.HasPrefix(contentType, "application/json") {
		var req struct {
			SessionID string `json:"session_id"`
			Key       string `json:"key"`
			S3Key     string `json:"s3_key"`
			FileName  string `json:"filename"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "Invalid JSON payload: "+err.Error(), http.StatusBadRequest)
			return
		}
		sessionID = strings.TrimSpace(req.SessionID)
		s3Key = strings.TrimSpace(req.Key)
		if s3Key == "" {
			s3Key = strings.TrimSpace(req.S3Key)
		}
		fileName = strings.TrimSpace(req.FileName)
	}

	// Check form fields if S3 key was not in JSON
	if s3Key == "" {
		_ = r.ParseMultipartForm(150 << 20)
		s3Key = strings.TrimSpace(r.FormValue("key"))
		if s3Key == "" {
			s3Key = strings.TrimSpace(r.FormValue("s3_key"))
		}
		if sessionID == "" {
			sessionID = strings.TrimSpace(r.FormValue("session_id"))
		}
		if fileName == "" {
			fileName = strings.TrimSpace(r.FormValue("filename"))
		}
	}

	if sessionID == "" {
		sessionID = generateSessionID()
	}

	sessionDir := filepath.Join(activeConfig.SessionsDir, sessionID)
	inputDir := filepath.Join(sessionDir, "input")
	if err := os.MkdirAll(inputDir, 0755); err != nil {
		jsonError(w, "Failed to create session dir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = cleaner.WriteSessionMeta(sessionDir, sessionID)

	var savedPath string

	if s3Key != "" && storage.IsConfigured() {
		// S3 Mode: download the object from S3 to stream into pdfcpu inspection
		if fileName == "" {
			fileName = filepath.Base(s3Key)
		}
		cleanName := filepath.Base(fileName)
		savedPath = filepath.Join(inputDir, cleanName)

		if err := storage.DefaultClient().DownloadObject(r.Context(), s3Key, savedPath); err != nil {
			jsonError(w, "Failed to fetch PDF from S3: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		// Local Fallback: standard multipart form file upload
		file, header, err := r.FormFile("file")
		if err != nil {
			jsonError(w, "No file provided and no S3 key specified", http.StatusBadRequest)
			return
		}
		defer file.Close()

		cleanName := filepath.Base(header.Filename)
		savedPath = filepath.Join(inputDir, cleanName)

		dst, err := os.Create(savedPath)
		if err != nil {
			jsonError(w, "Failed to write uploaded file: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := io.Copy(dst, file); err != nil {
			dst.Close()
			jsonError(w, "Failed to store file: "+err.Error(), http.StatusInternalServerError)
			return
		}
		dst.Close()

		// If S3 is configured, upload to S3 in background
		if storage.IsConfigured() {
			go func(sID, cName, localPath string) {
				targetKey := fmt.Sprintf("sessions/%s/input/%s", sID, cName)
				_ = storage.DefaultClient().UploadFile(context.Background(), targetKey, localPath, "application/pdf")
			}(sessionID, cleanName, savedPath)
		}
	}

	// Inspect the PDF
	info, err := splitter.InspectPDF(r.Context(), savedPath)
	if err != nil {
		_ = os.RemoveAll(sessionDir)
		jsonError(w, "PDF inspection failed: "+err.Error(), http.StatusBadRequest)
		return
	}

	info.SessionID = sessionID

	// Cache book info in session map
	sessionLock.Lock()
	sessionBooks[sessionID] = info
	sessionLock.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

func handleSplit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var sessionID string
	var pointsStr string
	var directFileUploaded bool

	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		var req struct {
			SessionID   string `json:"session_id"`
			SplitPoints string `json:"split_points"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			sessionID = strings.TrimSpace(req.SessionID)
			pointsStr = strings.TrimSpace(req.SplitPoints)
		}
	} else {
		if err := r.ParseMultipartForm(250 << 20); err == nil {
			sessionID = strings.TrimSpace(r.FormValue("session_id"))
			pointsStr = strings.TrimSpace(r.FormValue("split_points"))
		}
	}

	var inputPath string
	var baseFileName string

	// 1. Check in-memory session cache
	if sessionID != "" {
		sessionLock.RLock()
		cached, exists := sessionBooks[sessionID]
		sessionLock.RUnlock()
		if exists && cached != nil {
			inputPath = cached.Path
			baseFileName = cached.FileName
		}
	}

	sessionDir := filepath.Join(activeConfig.SessionsDir, sessionID)

	// 2. Check local session directory on disk
	if inputPath == "" && sessionID != "" {
		matches, _ := filepath.Glob(filepath.Join(sessionDir, "input", "*.pdf"))
		if len(matches) > 0 {
			inputPath = matches[0]
			baseFileName = filepath.Base(inputPath)
		}
	}

	// 3. Check S3 if enabled
	if inputPath == "" && sessionID != "" && storage.IsConfigured() {
		objs, err := storage.DefaultClient().ListObjects(r.Context(), fmt.Sprintf("sessions/%s/input/", sessionID))
		if err == nil && len(objs) > 0 {
			cleanName := filepath.Base(objs[0].Key)
			destPath := filepath.Join(sessionDir, "input", cleanName)
			if err := storage.DefaultClient().DownloadObject(r.Context(), objs[0].Key, destPath); err == nil {
				inputPath = destPath
				baseFileName = cleanName
			}
		}
	}

	// 4. Fallback to direct file upload in multipart form
	if inputPath == "" {
		file, header, err := r.FormFile("file")
		if err != nil {
			jsonError(w, "No active session or PDF file provided", http.StatusBadRequest)
			return
		}
		defer file.Close()
		directFileUploaded = true

		if sessionID == "" {
			sessionID = generateSessionID()
			sessionDir = filepath.Join(activeConfig.SessionsDir, sessionID)
		}
		inputDir := filepath.Join(sessionDir, "input")
		if err := os.MkdirAll(inputDir, 0755); err != nil {
			jsonError(w, "Failed to prepare session: "+err.Error(), http.StatusInternalServerError)
			return
		}
		_ = cleaner.WriteSessionMeta(sessionDir, sessionID)

		cleanName := filepath.Base(header.Filename)
		savedPath := filepath.Join(inputDir, cleanName)
		dst, err := os.Create(savedPath)
		if err != nil {
			jsonError(w, "Failed to write file: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := io.Copy(dst, file); err != nil {
			dst.Close()
			jsonError(w, "Failed to save upload: "+err.Error(), http.StatusInternalServerError)
			return
		}
		dst.Close()

		inputPath = savedPath
		baseFileName = cleanName
	}

	// Parse split points (comma or space separated)
	cleanPoints := strings.ReplaceAll(pointsStr, ",", " ")
	fields := strings.Fields(cleanPoints)
	var points []int
	for _, f := range fields {
		val, err := strconv.Atoi(f)
		if err != nil {
			jsonError(w, fmt.Sprintf("Invalid split number '%s'", f), http.StatusBadRequest)
			return
		}
		points = append(points, val)
	}

	// Prepare session parts directory
	partsDir := filepath.Join(sessionDir, "parts")
	if err := os.MkdirAll(partsDir, 0755); err != nil {
		jsonError(w, "Failed to create parts folder: "+err.Error(), http.StatusInternalServerError)
		return
	}

	opts := splitter.SplitOptions{
		InputPath:   inputPath,
		OutputDir:   partsDir,
		SplitPoints: points,
	}

	parts, err := splitter.SplitBook(r.Context(), opts, nil)
	if err != nil {
		jsonError(w, "Splitting failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Generate ZIP archive of all parts
	stem := strings.TrimSuffix(baseFileName, filepath.Ext(baseFileName))
	zipFileName := fmt.Sprintf("%s_split_parts.zip", stem)
	zipFilePath := filepath.Join(sessionDir, zipFileName)

	if err := splitter.CreateZipArchive(zipFilePath, parts); err != nil {
		log.Printf("[SERVER] Warning: failed to build zip archive: %v", err)
	}

	var zipFileSize int64
	if fi, err := os.Stat(zipFilePath); err == nil {
		zipFileSize = fi.Size()
	}

	zipDownloadURL := fmt.Sprintf("/api/download?session=%s&file=zip", sessionID)

	// If S3 is configured, upload split parts and ZIP to S3 and generate presigned GET URLs
	if storage.IsConfigured() {
		for i := range parts {
			partKey := fmt.Sprintf("sessions/%s/parts/%s", sessionID, parts[i].FileName)
			if err := storage.DefaultClient().UploadFile(r.Context(), partKey, parts[i].FilePath, "application/pdf"); err == nil {
				if getURL, err := storage.DefaultClient().GeneratePresignedGetURL(r.Context(), partKey, 24*time.Hour); err == nil {
					parts[i].DownloadURL = getURL
				} else {
					parts[i].DownloadURL = fmt.Sprintf("/api/download?session=%s&file=%s", sessionID, parts[i].FileName)
				}
			} else {
				parts[i].DownloadURL = fmt.Sprintf("/api/download?session=%s&file=%s", sessionID, parts[i].FileName)
			}
		}

		zipKey := fmt.Sprintf("sessions/%s/%s", sessionID, zipFileName)
		if err := storage.DefaultClient().UploadFile(r.Context(), zipKey, zipFilePath, "application/zip"); err == nil {
			if getURL, err := storage.DefaultClient().GeneratePresignedGetURL(r.Context(), zipKey, 24*time.Hour); err == nil {
				zipDownloadURL = getURL
			}
		}

		if directFileUploaded {
			inputKey := fmt.Sprintf("sessions/%s/input/%s", sessionID, baseFileName)
			go func(k, p string) {
				_ = storage.DefaultClient().UploadFile(context.Background(), k, p, "application/pdf")
			}(inputKey, inputPath)
		}
	} else {
		for i := range parts {
			parts[i].DownloadURL = fmt.Sprintf("/api/download?session=%s&file=%s", sessionID, parts[i].FileName)
		}
	}

	resp := map[string]interface{}{
		"success":           true,
		"session_id":        sessionID,
		"s3_enabled":        storage.IsConfigured(),
		"output_dir":        partsDir,
		"parts":             parts,
		"zip_filename":      zipFileName,
		"zip_size":          zipFileSize,
		"zip_download_url":  zipDownloadURL,
		"retention_minutes": int(activeConfig.Retention.Minutes()),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	sessionID := filepath.Clean(r.URL.Query().Get("session"))
	fileName := r.URL.Query().Get("file")

	if sessionID == "" || fileName == "" || strings.Contains(sessionID, "..") || strings.Contains(fileName, "..") || strings.Contains(sessionID, "/") || strings.Contains(sessionID, "\\") {
		http.Error(w, "Invalid download parameters", http.StatusBadRequest)
		return
	}

	sessionDir := filepath.Join(activeConfig.SessionsDir, sessionID)

	var targetPath string
	var downloadName string
	var s3Key string

	if fileName == "zip" {
		matches, err := filepath.Glob(filepath.Join(sessionDir, "*.zip"))
		if err == nil && len(matches) > 0 {
			targetPath = matches[0]
			downloadName = filepath.Base(targetPath)
		}
		if targetPath == "" && storage.IsConfigured() {
			if objs, err := storage.DefaultClient().ListObjects(r.Context(), fmt.Sprintf("sessions/%s/", sessionID)); err == nil {
				for _, obj := range objs {
					if strings.HasSuffix(obj.Key, ".zip") {
						s3Key = obj.Key
						downloadName = filepath.Base(obj.Key)
						break
					}
				}
			}
		}
	} else {
		cleanFile := filepath.Base(fileName)
		localCandidate := filepath.Join(sessionDir, "parts", cleanFile)
		if _, err := os.Stat(localCandidate); err == nil {
			targetPath = localCandidate
			downloadName = cleanFile
		} else if storage.IsConfigured() {
			s3Key = fmt.Sprintf("sessions/%s/parts/%s", sessionID, cleanFile)
			downloadName = cleanFile
		}
	}

	// If local file is available, serve it
	if targetPath != "" {
		fi, err := os.Stat(targetPath)
		if err == nil {
			f, err := os.Open(targetPath)
			if err == nil {
				defer f.Close()
				w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, downloadName))
				w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
				if strings.HasSuffix(downloadName, ".zip") {
					w.Header().Set("Content-Type", "application/zip")
				} else {
					w.Header().Set("Content-Type", "application/pdf")
				}
				http.ServeContent(w, r, downloadName, fi.ModTime(), f)
				return
			}
		}
	}

	// If file is in S3, redirect to presigned GET URL
	if s3Key != "" && storage.IsConfigured() {
		presignedURL, err := storage.DefaultClient().GeneratePresignedGetURL(r.Context(), s3Key, 1*time.Hour)
		if err == nil {
			http.Redirect(w, r, presignedURL, http.StatusFound)
			return
		}
	}

	http.Error(w, "Requested file not found or expired", http.StatusNotFound)
}

// handleClear immediately purges a session's data from both local disk and S3.
// It supports invocation via fetch, form POST, or navigator.sendBeacon on page unload.
func handleClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var sessionID string

	// 1. Check Query params
	sessionID = strings.TrimSpace(r.URL.Query().Get("session_id"))
	if sessionID == "" {
		sessionID = strings.TrimSpace(r.URL.Query().Get("session"))
	}

	// 2. Check JSON, URL-encoded form, or sendBeacon raw body
	if sessionID == "" && r.Body != nil {
		bodyBytes, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
		bodyStr := strings.TrimSpace(string(bodyBytes))

		if strings.HasPrefix(bodyStr, "{") {
			var req struct {
				SessionID string `json:"session_id"`
				Session   string `json:"session"`
			}
			if err := json.Unmarshal(bodyBytes, &req); err == nil {
				sessionID = strings.TrimSpace(req.SessionID)
				if sessionID == "" {
					sessionID = strings.TrimSpace(req.Session)
				}
			}
		} else if strings.Contains(bodyStr, "=") {
			if vals, err := url.ParseQuery(bodyStr); err == nil {
				sessionID = strings.TrimSpace(vals.Get("session_id"))
				if sessionID == "" {
					sessionID = strings.TrimSpace(vals.Get("session"))
				}
			}
		} else if len(bodyStr) > 0 && len(bodyStr) <= 64 && !strings.Contains(bodyStr, "\n") {
			sessionID = bodyStr
		}
	}

	// 3. Fallback to standard parsed form
	if sessionID == "" {
		_ = r.ParseForm()
		sessionID = strings.TrimSpace(r.FormValue("session_id"))
		if sessionID == "" {
			sessionID = strings.TrimSpace(r.FormValue("session"))
		}
	}

	// Sanitize session ID
	sessionID = filepath.Clean(sessionID)
	if sessionID == "" || strings.Contains(sessionID, ".") || strings.Contains(sessionID, "/") || strings.Contains(sessionID, "\\") {
		jsonError(w, "Valid session_id required", http.StatusBadRequest)
		return
	}

	// Purge from memory cache
	sessionLock.Lock()
	delete(sessionBooks, sessionID)
	sessionLock.Unlock()

	// Purge from local disk storage
	sessionDir := filepath.Join(activeConfig.SessionsDir, sessionID)
	_ = os.RemoveAll(sessionDir)

	// Purge from S3 storage if configured
	if storage.IsConfigured() {
		go func(sID string) {
			ctxTimeout, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			_, _ = storage.DefaultClient().DeletePrefix(ctxTimeout, fmt.Sprintf("sessions/%s/", sID))
		}(sessionID)
	}

	log.Printf("[SERVER] Cleared session immediately: %s", sessionID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"session_id": sessionID,
		"message":    "Session data purged successfully",
	})
}

// handleDownloadApp serves compiled standalone binary files for desktop/server use.
func handleDownloadApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	platform := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("platform")))

	// Detect platform from User-Agent if not explicitly specified
	if platform == "" {
		ua := strings.ToLower(r.UserAgent())
		if strings.Contains(ua, "linux") {
			platform = "linux"
		} else if strings.Contains(ua, "mac") || strings.Contains(ua, "darwin") {
			platform = "darwin"
		} else {
			platform = "windows"
		}
	}

	if platform == "darwin" || platform == "mac" || platform == "macos" {
		http.Error(w, "macOS build coming soon", http.StatusNotFound)
		return
	}

	var binaryName string
	if platform == "linux" || platform == "linux-amd64" {
		binaryName = "pdf-plitter-linux-amd64"
	} else {
		binaryName = "pdf-plitter.exe"
	}

	binaryPath := findApplicationBinary(binaryName)
	if binaryPath == "" {
		http.Error(w, fmt.Sprintf("Binary '%s' not found on server", binaryName), http.StatusNotFound)
		return
	}

	fi, err := os.Stat(binaryPath)
	if err != nil {
		http.Error(w, "Failed to read binary file", http.StatusInternalServerError)
		return
	}

	f, err := os.Open(binaryPath)
	if err != nil {
		http.Error(w, "Failed to open binary file", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, binaryName))
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.Header().Set("Content-Type", "application/octet-stream")

	http.ServeContent(w, r, binaryName, fi.ModTime(), f)
}

func findApplicationBinary(name string) string {
	var candidates []string

	// 1. Next to current running executable and inside its dist/build subfolder
	if execPath, err := os.Executable(); err == nil {
		execDir := filepath.Dir(execPath)
		candidates = append(candidates, filepath.Join(execDir, "dist", name))
		candidates = append(candidates, filepath.Join(execDir, "build", name))
		candidates = append(candidates, filepath.Join(execDir, name))
	}

	// 2. In dist/ or build/ under current working directory
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "dist", name))
		candidates = append(candidates, filepath.Join(cwd, "build", name))
		candidates = append(candidates, filepath.Join(cwd, name))
		candidates = append(candidates, filepath.Join(cwd, "tools", "pdf-plitter", "dist", name))
		candidates = append(candidates, filepath.Join(cwd, "tools", "pdf-plitter", name))
	}

	// 3. Known repository dist/build location
	candidates = append(candidates, filepath.Join("D:\\WORKSPACE\\PROJECTS\\school-lessons\\tools\\pdf-plitter\\dist", name))
	candidates = append(candidates, filepath.Join("D:\\WORKSPACE\\PROJECTS\\school-lessons\\tools\\pdf-plitter\\build", name))
	candidates = append(candidates, filepath.Join("D:\\WORKSPACE\\PROJECTS\\school-lessons\\tools\\pdf-plitter", name))

	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() && fi.Size() > 0 {
			return c
		}
	}

	return ""
}

func handleOpenFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Folder string `json:"folder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Folder == "" {
		jsonError(w, "Folder path required", http.StatusBadRequest)
		return
	}

	// Only attempt explorer if running locally with desktop support
	if !activeConfig.Headless && (runtime.GOOS == "windows" || runtime.GOOS == "darwin" || os.Getenv("DISPLAY") != "") {
		go openExplorer(req.Folder)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return // Headless Linux environment
		}
		cmd = exec.Command("xdg-open", url)
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
