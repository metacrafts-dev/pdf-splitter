package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"pdf-splitter/cleaner"
	"pdf-splitter/splitter"
)

func (s *Server) handleSplit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var (
		sessionID          string
		pointsStr          string
		directFileUploaded bool
	)

	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		r.Body = http.MaxBytesReader(w, r.Body, controlBodyLimit)
		var req struct {
			SessionID   string `json:"session_id"`
			SplitPoints string `json:"split_points"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "Invalid JSON payload", http.StatusBadRequest)
			return
		}
		sessionID = strings.TrimSpace(req.SessionID)
		pointsStr = strings.TrimSpace(req.SplitPoints)
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+(10<<20))
		if err := r.ParseMultipartForm(multipartMemMax); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				jsonError(w, fmt.Sprintf("File exceeds maximum allowed size of %d MB", s.cfg.MaxUploadBytes/(1024*1024)), http.StatusRequestEntityTooLarge)
				return
			}
			jsonError(w, "Failed to parse split request", http.StatusBadRequest)
			return
		}
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()
		sessionID = strings.TrimSpace(r.FormValue("session_id"))
		pointsStr = strings.TrimSpace(r.FormValue("split_points"))
	}

	// Parse and validate split points early before doing any disk/CPU work.
	cleanPoints := strings.ReplaceAll(pointsStr, ",", " ")
	fields := strings.Fields(cleanPoints)
	if len(fields) > splitter.MaxSplitPoints {
		jsonError(w, fmt.Sprintf("Too many split points (maximum %d allowed)", splitter.MaxSplitPoints), http.StatusBadRequest)
		return
	}
	points := make([]int, 0, len(fields))
	for _, f := range fields {
		val, err := strconv.Atoi(f)
		if err != nil {
			jsonError(w, fmt.Sprintf("Invalid split number '%s'", f), http.StatusBadRequest)
			return
		}
		points = append(points, val)
	}

	var (
		inputPath    string
		baseFileName string
		sessionDir   string
		err          error
	)

	if sessionID != "" {
		sessionDir, err = s.safeSessionDir(sessionID)
		if err != nil {
			jsonError(w, "Invalid session_id", http.StatusBadRequest)
			return
		}

		// 1. Check in-memory session cache
		s.sessionsMu.RLock()
		rec, exists := s.sessions[sessionID]
		s.sessionsMu.RUnlock()
		if exists && rec != nil && rec.Book != nil {
			if _, statErr := os.Stat(rec.Book.Path); statErr == nil {
				inputPath = rec.Book.Path
				baseFileName = rec.Book.FileName
			}
		}

		// 2. Check local session input directory case-insensitively (safe against glob chars / .PDF)
		if inputPath == "" {
			inputPath, baseFileName = findFirstPDFInDir(filepath.Join(sessionDir, "input"))
		}

		// 3. Check S3 if enabled
		if inputPath == "" && s.s3Configured() {
			objs, listErr := s.store.ListObjects(r.Context(), fmt.Sprintf("sessions/%s/input/", sessionID))
			if listErr == nil && len(objs) > 0 {
				cleanName := sanitizeFilename(filepath.Base(objs[0].Key))
				inputDir := filepath.Join(sessionDir, "input")
				destPath, pathErr := ensureContainedPath(inputDir, filepath.Join(inputDir, cleanName))
				if pathErr == nil {
					if dlErr := s.store.DownloadObjectWithLimit(r.Context(), objs[0].Key, destPath, s.cfg.MaxUploadBytes); dlErr == nil {
						inputPath = destPath
						baseFileName = cleanName
					}
				}
			}
		}
	}

	// 4. Fallback to direct file upload in multipart form
	if inputPath == "" {
		file, header, formErr := r.FormFile("file")
		if formErr != nil {
			jsonError(w, "No active session or PDF file provided", http.StatusBadRequest)
			return
		}
		defer file.Close()
		directFileUploaded = true

		cleanName := sanitizeFilename(header.Filename)
		if !strings.HasSuffix(strings.ToLower(cleanName), ".pdf") {
			jsonError(w, "Only PDF files are supported", http.StatusBadRequest)
			return
		}

		sessionID, err = generateSessionID()
		if err != nil {
			jsonError(w, "Failed to initialize session", http.StatusInternalServerError)
			return
		}
		sessionDir, err = s.safeSessionDir(sessionID)
		if err != nil {
			jsonError(w, "Failed to prepare session", http.StatusInternalServerError)
			return
		}
		inputDir := filepath.Join(sessionDir, "input")
		if err := os.MkdirAll(inputDir, 0750); err != nil {
			jsonError(w, "Failed to prepare session directory", http.StatusInternalServerError)
			return
		}
		_ = cleaner.WriteSessionMeta(sessionDir, sessionID)

		savedPath, pathErr := ensureContainedPath(inputDir, filepath.Join(inputDir, cleanName))
		if pathErr != nil {
			_ = os.RemoveAll(sessionDir)
			jsonError(w, "Invalid filename", http.StatusBadRequest)
			return
		}

		dst, createErr := os.Create(savedPath)
		if createErr != nil {
			_ = os.RemoveAll(sessionDir)
			jsonError(w, "Failed to save uploaded file", http.StatusInternalServerError)
			return
		}
		written, copyErr := io.Copy(dst, io.LimitReader(file, s.cfg.MaxUploadBytes+1))
		closeErr := dst.Close()
		if copyErr != nil || closeErr != nil || written > s.cfg.MaxUploadBytes {
			_ = os.RemoveAll(sessionDir)
			jsonError(w, "Failed to store uploaded file", http.StatusBadRequest)
			return
		}

		inputPath = savedPath
		baseFileName = cleanName
	}

	// Acquire split concurrency semaphore to prevent CPU/RAM exhaustion.
	select {
	case s.splitSem <- struct{}{}:
		defer func() { <-s.splitSem }()
	case <-r.Context().Done():
		jsonError(w, "Request cancelled", http.StatusRequestTimeout)
		return
	}

	// Clean any previous split parts and stale ZIP archives before re-splitting the same session.
	partsDir := filepath.Join(sessionDir, "parts")
	_ = os.RemoveAll(partsDir)
	removeZipFilesInDir(sessionDir)
	if err := os.MkdirAll(partsDir, 0750); err != nil {
		jsonError(w, "Failed to prepare parts directory", http.StatusInternalServerError)
		return
	}

	parts, err := splitter.SplitBook(r.Context(), splitter.SplitOptions{
		InputPath:   inputPath,
		OutputDir:   partsDir,
		SplitPoints: points,
	}, nil)
	if err != nil {
		slog.Warn("PDF split failed", "session_id", sessionID, "err", err)
		jsonError(w, "Splitting failed: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Build ZIP archive atomically in Store mode.
	stem := strings.TrimSuffix(baseFileName, filepath.Ext(baseFileName))
	if stem == "" {
		stem = "book"
	}
	zipFileName := sanitizeFilename(fmt.Sprintf("%s_split_parts.zip", stem))
	zipFilePath := filepath.Join(sessionDir, zipFileName)

	var (
		zipFileSize    int64
		zipDownloadURL string
	)
	if err := splitter.CreateZipArchive(zipFilePath, parts); err != nil {
		slog.Error("failed to build zip archive", "session_id", sessionID, "err", err)
	} else if fi, statErr := os.Stat(zipFilePath); statErr == nil {
		zipFileSize = fi.Size()
		zipDownloadURL = fmt.Sprintf("/api/download?session=%s&file=zip", url.QueryEscape(sessionID))
	}

	// Align S3 download URL TTL with session retention (minimum 15m).
	presignTTL := s.cfg.Retention
	if presignTTL < 15*time.Minute {
		presignTTL = 15 * time.Minute
	}

	if s.s3Configured() {
		// Upload parts concurrently with bounded parallelism (4 workers).
		sem := make(chan struct{}, 4)
		var wg sync.WaitGroup
		for i := range parts {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				partKey := fmt.Sprintf("sessions/%s/parts/%s", sessionID, parts[idx].FileName)
				if upErr := s.store.UploadFile(r.Context(), partKey, parts[idx].FilePath, "application/pdf"); upErr == nil {
					if getURL, urlErr := s.store.GeneratePresignedDownloadURL(r.Context(), partKey, parts[idx].FileName, presignTTL); urlErr == nil {
						parts[idx].DownloadURL = getURL
						return
					}
				}
				parts[idx].DownloadURL = fmt.Sprintf("/api/download?session=%s&file=%s",
					url.QueryEscape(sessionID), url.QueryEscape(parts[idx].FileName))
			}(i)
		}
		wg.Wait()

		if zipDownloadURL != "" {
			zipKey := fmt.Sprintf("sessions/%s/%s", sessionID, zipFileName)
			if upErr := s.store.UploadFile(r.Context(), zipKey, zipFilePath, "application/zip"); upErr == nil {
				if getURL, urlErr := s.store.GeneratePresignedDownloadURL(r.Context(), zipKey, zipFileName, presignTTL); urlErr == nil {
					zipDownloadURL = getURL
				}
			}
		}

		if directFileUploaded {
			s.bgWG.Add(1)
			go func(k, p string) {
				defer s.bgWG.Done()
				ctxTimeout, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				_ = s.store.UploadFile(ctxTimeout, k, p, "application/pdf")
			}(fmt.Sprintf("sessions/%s/input/%s", sessionID, baseFileName), inputPath)
		}
	} else {
		for i := range parts {
			parts[i].DownloadURL = fmt.Sprintf("/api/download?session=%s&file=%s",
				url.QueryEscape(sessionID), url.QueryEscape(parts[i].FileName))
		}
	}

	// Scrub internal filesystem paths before returning JSON response.
	for i := range parts {
		parts[i].FilePath = ""
	}

	resp := map[string]any{
		"success":           true,
		"session_id":        sessionID,
		"s3_enabled":        s.s3Configured(),
		"can_open_folder":   s.cfg.Standalone && !s.cfg.Headless,
		"parts":             parts,
		"zip_filename":      zipFileName,
		"zip_size":          zipFileSize,
		"zip_download_url":  zipDownloadURL,
		"retention_minutes": int(s.cfg.Retention.Minutes()),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := strings.TrimSpace(r.URL.Query().Get("session"))
	fileName := strings.TrimSpace(r.URL.Query().Get("file"))

	sessionDir, err := s.safeSessionDir(sessionID)
	if err != nil || fileName == "" {
		http.Error(w, "Invalid download parameters", http.StatusBadRequest)
		return
	}

	var (
		targetPath   string
		downloadName string
		s3Key        string
	)

	if fileName == "zip" {
		targetPath, downloadName = findFirstZipInDir(sessionDir)
		if targetPath == "" && s.s3Configured() {
			if objs, listErr := s.store.ListObjects(r.Context(), fmt.Sprintf("sessions/%s/", sessionID)); listErr == nil {
				for _, obj := range objs {
					if strings.HasSuffix(strings.ToLower(obj.Key), ".zip") {
						s3Key = obj.Key
						downloadName = sanitizeFilename(filepath.Base(obj.Key))
						break
					}
				}
			}
		}
	} else {
		cleanFile := sanitizeFilename(fileName)
		partsDir := filepath.Join(sessionDir, "parts")
		localCandidate, pathErr := ensureContainedPath(partsDir, filepath.Join(partsDir, cleanFile))
		if pathErr != nil {
			http.Error(w, "Invalid file parameter", http.StatusBadRequest)
			return
		}
		if _, statErr := os.Stat(localCandidate); statErr == nil {
			targetPath = localCandidate
			downloadName = cleanFile
		} else if s.s3Configured() {
			s3Key = fmt.Sprintf("sessions/%s/parts/%s", sessionID, cleanFile)
			downloadName = cleanFile
		}
	}

	if targetPath != "" {
		fi, statErr := os.Stat(targetPath)
		if statErr == nil && !fi.IsDir() {
			f, openErr := os.Open(targetPath)
			if openErr == nil {
				defer f.Close()
				cd := mime.FormatMediaType("attachment", map[string]string{"filename": downloadName})
				if cd != "" {
					w.Header().Set("Content-Disposition", cd)
				}
				if strings.HasSuffix(strings.ToLower(downloadName), ".zip") {
					w.Header().Set("Content-Type", "application/zip")
				} else {
					w.Header().Set("Content-Type", "application/pdf")
				}
				// Let http.ServeContent set Content-Length properly (supports HTTP Range requests).
				http.ServeContent(w, r, downloadName, fi.ModTime(), f)
				return
			}
		}
	}

	if s3Key != "" && s.s3Configured() {
		presignTTL := s.cfg.Retention
		if presignTTL < 15*time.Minute {
			presignTTL = 15 * time.Minute
		}
		presignedURL, urlErr := s.store.GeneratePresignedDownloadURL(r.Context(), s3Key, downloadName, presignTTL)
		if urlErr == nil {
			http.Redirect(w, r, presignedURL, http.StatusFound)
			return
		}
	}

	http.Error(w, "Requested file not found or expired", http.StatusNotFound)
}

// handleClear immediately purges a session's data from memory, local disk, and S3.
// Only POST and DELETE are permitted (GET is disallowed to prevent destructive safe-method requests).
func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, controlBodyLimit)
	sessionID := extractSessionIDFromRequest(r)

	sessionDir, err := s.safeSessionDir(sessionID)
	if err != nil {
		jsonError(w, "Valid session_id required", http.StatusBadRequest)
		return
	}

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

	slog.Info("cleared session immediately", "session_id", sessionID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success":    true,
		"session_id": sessionID,
		"message":    "Session data purged successfully",
	})
}

// handleDownloadApp serves compiled standalone binaries located alongside the running executable or dist/.
func (s *Server) handleDownloadApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	platform := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("platform")))
	if platform == "" {
		ua := strings.ToLower(r.UserAgent())
		switch {
		case strings.Contains(ua, "linux"):
			platform = "linux"
		case strings.Contains(ua, "mac") || strings.Contains(ua, "darwin"):
			platform = "darwin"
		default:
			platform = "windows"
		}
	}

	if platform == "darwin" || platform == "mac" || platform == "macos" {
		http.Error(w, "macOS build coming soon", http.StatusNotFound)
		return
	}

	binaryName := "pdf-splitter.exe"
	if platform == "linux" || platform == "linux-amd64" {
		binaryName = "pdf-splitter-linux-amd64"
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

	cd := mime.FormatMediaType("attachment", map[string]string{"filename": binaryName})
	w.Header().Set("Content-Disposition", cd)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, binaryName, fi.ModTime(), f)
}

func findApplicationBinary(name string) string {
	var candidates []string

	if execPath, err := os.Executable(); err == nil {
		execDir := filepath.Dir(execPath)
		candidates = append(candidates,
			filepath.Join(execDir, "dist", name),
			filepath.Join(execDir, "build", name),
			filepath.Join(execDir, name),
		)
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, "dist", name),
			filepath.Join(cwd, "build", name),
			filepath.Join(cwd, name),
		)
	}

	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() && fi.Size() > 0 {
			return c
		}
	}
	return ""
}

func findFirstPDFInDir(dir string) (string, string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", ""
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".pdf") {
			return filepath.Join(dir, e.Name()), e.Name()
		}
	}
	return "", ""
}

func findFirstZipInDir(dir string) (string, string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", ""
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".zip") {
			return filepath.Join(dir, e.Name()), e.Name()
		}
	}
	return "", ""
}

func removeZipFilesInDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".zip") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func extractSessionIDFromRequest(r *http.Request) string {
	if sid := strings.TrimSpace(r.URL.Query().Get("session_id")); sid != "" {
		return sid
	}
	if sid := strings.TrimSpace(r.URL.Query().Get("session")); sid != "" {
		return sid
	}
	if r.Body != nil {
		bodyBytes, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
		bodyStr := strings.TrimSpace(string(bodyBytes))
		if strings.HasPrefix(bodyStr, "{") {
			var req struct {
				SessionID string `json:"session_id"`
				Session   string `json:"session"`
			}
			if err := json.Unmarshal(bodyBytes, &req); err == nil {
				if s := strings.TrimSpace(req.SessionID); s != "" {
					return s
				}
				return strings.TrimSpace(req.Session)
			}
		} else if strings.Contains(bodyStr, "=") {
			if vals, err := url.ParseQuery(bodyStr); err == nil {
				if s := strings.TrimSpace(vals.Get("session_id")); s != "" {
					return s
				}
				return strings.TrimSpace(vals.Get("session"))
			}
		} else if len(bodyStr) == 32 {
			return bodyStr
		}
	}
	return ""
}
