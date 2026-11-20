package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pdf-splitter/cleaner"
	"pdf-splitter/splitter"
)

// handleConfig returns public server capabilities without exposing internal S3 bucket/region names.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp := map[string]any{
		"s3_enabled":        s.s3Configured(),
		"retention_minutes": int(s.cfg.Retention.Minutes()),
		"max_upload_mb":     int(s.cfg.MaxUploadBytes / (1024 * 1024)),
		"standalone":        s.cfg.Standalone,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleS3Presign generates a size- and content-type-constrained presigned S3 PUT URL
// and registers the server-minted session so /api/inspect only inspects authorized keys.
func (s *Server) handleS3Presign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.s3Configured() {
		jsonError(w, "S3 storage is not configured", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, controlBodyLimit)

	var filename string
	var fileSize int64

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		var req struct {
			FileName string `json:"filename"`
			Name     string `json:"name"`
			FileSize int64  `json:"file_size"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "Invalid JSON payload", http.StatusBadRequest)
			return
		}
		filename = req.FileName
		if filename == "" {
			filename = req.Name
		}
		fileSize = req.FileSize
	} else {
		if err := r.ParseForm(); err != nil {
			jsonError(w, "Invalid form payload", http.StatusBadRequest)
			return
		}
		filename = r.FormValue("filename")
		if filename == "" {
			filename = r.FormValue("name")
		}
	}

	if fileSize <= 0 || fileSize > s.cfg.MaxUploadBytes {
		jsonError(w, fmt.Sprintf("File size must be between 1 byte and %d MB", s.cfg.MaxUploadBytes/(1024*1024)), http.StatusBadRequest)
		return
	}

	cleanName := sanitizeFilename(filename)
	if !strings.HasSuffix(strings.ToLower(cleanName), ".pdf") {
		jsonError(w, "Only PDF files are supported", http.StatusBadRequest)
		return
	}

	sessionID, err := generateSessionID()
	if err != nil {
		jsonError(w, "Failed to initialize session", http.StatusInternalServerError)
		return
	}

	key := fmt.Sprintf("sessions/%s/input/%s", sessionID, cleanName)
	const pdfContentType = "application/pdf"

	uploadURL, err := s.store.GeneratePresignedPutURLWithConstraints(
		r.Context(), key, 15*time.Minute, fileSize, pdfContentType,
	)
	if err != nil {
		slog.Error("failed to generate presigned PUT URL", "err", err)
		jsonError(w, "Failed to generate presigned upload URL", http.StatusInternalServerError)
		return
	}

	s.sessionsMu.Lock()
	s.sessions[sessionID] = &sessionRecord{
		SessionID:     sessionID,
		ExpectedS3Key: key,
		ExpectedSize:  fileSize,
		FileName:      cleanName,
		CreatedAt:     time.Now().UTC(),
	}
	s.sessionsMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success":      true,
		"session_id":   sessionID,
		"key":          key,
		"filename":     cleanName,
		"upload_url":   uploadURL,
		"url":          uploadURL,
		"content_type": pdfContentType,
	})
}

func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	contentType := r.Header.Get("Content-Type")
	var (
		sessionID string
		s3Key     string
		fileName  string
	)

	if strings.HasPrefix(contentType, "application/json") {
		r.Body = http.MaxBytesReader(w, r.Body, controlBodyLimit)
		var req struct {
			SessionID string `json:"session_id"`
			Key       string `json:"key"`
			S3Key     string `json:"s3_key"`
			FileName  string `json:"filename"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "Invalid JSON payload", http.StatusBadRequest)
			return
		}
		sessionID = strings.TrimSpace(req.SessionID)
		s3Key = strings.TrimSpace(req.Key)
		if s3Key == "" {
			s3Key = strings.TrimSpace(req.S3Key)
		}
		fileName = strings.TrimSpace(req.FileName)
	} else {
		// Enforce hard upper bound on multipart upload body and spill >32 MiB to temp files.
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+(10<<20))
		if err := r.ParseMultipartForm(multipartMemMax); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				jsonError(w, fmt.Sprintf("File exceeds maximum allowed size of %d MB", s.cfg.MaxUploadBytes/(1024*1024)), http.StatusRequestEntityTooLarge)
				return
			}
			jsonError(w, "Failed to parse upload request", http.StatusBadRequest)
			return
		}
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()

		s3Key = strings.TrimSpace(r.FormValue("key"))
		if s3Key == "" {
			s3Key = strings.TrimSpace(r.FormValue("s3_key"))
		}
		sessionID = strings.TrimSpace(r.FormValue("session_id"))
		fileName = strings.TrimSpace(r.FormValue("filename"))
		if fileName == "" {
			fileName = strings.TrimSpace(r.FormValue("file_name"))
		}
	}

	var savedPath string
	var sessionDir string

	if s3Key != "" && s.s3Configured() {
		// S3 mode: sessionID MUST match a server-minted presigned session with this exact s3Key.
		if !isValidSessionID(sessionID) {
			jsonError(w, "Invalid session_id", http.StatusBadRequest)
			return
		}

		s.sessionsMu.RLock()
		rec, exists := s.sessions[sessionID]
		s.sessionsMu.RUnlock()

		if !exists || rec.ExpectedS3Key == "" || rec.ExpectedS3Key != s3Key {
			jsonError(w, "Unauthorized or unrecognized S3 object key for session", http.StatusForbidden)
			return
		}

		var err error
		sessionDir, err = s.safeSessionDir(sessionID)
		if err != nil {
			jsonError(w, "Invalid session directory", http.StatusBadRequest)
			return
		}
		inputDir := filepath.Join(sessionDir, "input")
		if err := os.MkdirAll(inputDir, 0750); err != nil {
			jsonError(w, "Failed to create session directory", http.StatusInternalServerError)
			return
		}
		_ = cleaner.WriteSessionMeta(sessionDir, sessionID)

		cleanName := rec.FileName
		if cleanName == "" {
			cleanName = sanitizeFilename(fileName)
		}
		savedPath, err = ensureContainedPath(inputDir, filepath.Join(inputDir, cleanName))
		if err != nil {
			_ = os.RemoveAll(sessionDir)
			jsonError(w, "Invalid filename", http.StatusBadRequest)
			return
		}

		if err := s.store.DownloadObjectWithLimit(r.Context(), s3Key, savedPath, s.cfg.MaxUploadBytes); err != nil {
			_ = os.RemoveAll(sessionDir)
			slog.Warn("failed to fetch PDF from S3", "session_id", sessionID, "err", err)
			jsonError(w, "Failed to fetch uploaded PDF from storage", http.StatusBadRequest)
			return
		}
	} else {
		// Local upload mode: always mint a fresh server-side sessionID (ignore any client-supplied ID).
		file, header, err := r.FormFile("file")
		if err != nil {
			jsonError(w, "No PDF file provided", http.StatusBadRequest)
			return
		}
		defer file.Close()

		cleanName := sanitizeFilename(header.Filename)
		if !strings.HasSuffix(strings.ToLower(cleanName), ".pdf") {
			jsonError(w, "Only PDF files are supported", http.StatusBadRequest)
			return
		}

		sessionID, err = generateSessionID()
		if err != nil {
			jsonError(w, "Failed to create session", http.StatusInternalServerError)
			return
		}

		sessionDir, err = s.safeSessionDir(sessionID)
		if err != nil {
			jsonError(w, "Failed to prepare session", http.StatusInternalServerError)
			return
		}
		inputDir := filepath.Join(sessionDir, "input")
		if err := os.MkdirAll(inputDir, 0750); err != nil {
			jsonError(w, "Failed to create session directory", http.StatusInternalServerError)
			return
		}
		_ = cleaner.WriteSessionMeta(sessionDir, sessionID)

		savedPath, err = ensureContainedPath(inputDir, filepath.Join(inputDir, cleanName))
		if err != nil {
			_ = os.RemoveAll(sessionDir)
			jsonError(w, "Invalid filename", http.StatusBadRequest)
			return
		}

		dst, err := os.Create(savedPath)
		if err != nil {
			_ = os.RemoveAll(sessionDir)
			jsonError(w, "Failed to save uploaded file", http.StatusInternalServerError)
			return
		}
		written, copyErr := io.Copy(dst, io.LimitReader(file, s.cfg.MaxUploadBytes+1))
		closeErr := dst.Close()
		if copyErr != nil || closeErr != nil {
			_ = os.RemoveAll(sessionDir)
			jsonError(w, "Failed to store uploaded file", http.StatusInternalServerError)
			return
		}
		if written > s.cfg.MaxUploadBytes {
			_ = os.RemoveAll(sessionDir)
			jsonError(w, fmt.Sprintf("File exceeds maximum allowed size of %d MB", s.cfg.MaxUploadBytes/(1024*1024)), http.StatusRequestEntityTooLarge)
			return
		}
	}

	// Validate PDF header and inspect page count before accepting or syncing to S3.
	info, err := splitter.InspectPDF(r.Context(), savedPath)
	if err != nil {
		_ = os.RemoveAll(sessionDir)
		s.evictSession(sessionID)
		jsonError(w, "Invalid PDF file: "+err.Error(), http.StatusBadRequest)
		return
	}

	info.SessionID = sessionID

	s.sessionsMu.Lock()
	s.sessions[sessionID] = &sessionRecord{
		SessionID: sessionID,
		Book:      info,
		FileName:  info.FileName,
		CreatedAt: time.Now().UTC(),
	}
	s.sessionsMu.Unlock()

	// If local fallback upload occurred while S3 is enabled, mirror asynchronously with tracked WaitGroup.
	if s3Key == "" && s.s3Configured() {
		s.bgWG.Add(1)
		go func(sID, cName, localPath string) {
			defer s.bgWG.Done()
			ctxTimeout, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			targetKey := fmt.Sprintf("sessions/%s/input/%s", sID, cName)
			if err := s.store.UploadFile(ctxTimeout, targetKey, localPath, "application/pdf"); err != nil {
				slog.Warn("background S3 input mirror failed", "session_id", sID, "err", err)
			}
		}(sessionID, info.FileName, savedPath)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}
