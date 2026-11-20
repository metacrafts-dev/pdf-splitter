package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pdf-plitter/storage"
)

func TestHandleConfig(t *testing.T) {
	activeConfig = ServerConfig{
		Retention:  15 * time.Minute,
		Standalone: true,
	}

	// 1. Without S3 configured
	storage.SetDefaultClient(storage.NewClient(storage.Config{}))

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode json: %v", err)
	}

	if resp["s3_enabled"] != false {
		t.Errorf("expected s3_enabled to be false, got %v", resp["s3_enabled"])
	}
	if resp["standalone"] != true {
		t.Errorf("expected standalone to be true, got %v", resp["standalone"])
	}
	if int(resp["retention_minutes"].(float64)) != 15 {
		t.Errorf("expected retention_minutes to be 15, got %v", resp["retention_minutes"])
	}

	// 2. With S3 configured
	storage.SetDefaultClient(storage.NewClient(storage.Config{
		Bucket:          "test-bucket",
		Region:          "us-east-1",
		AccessKeyID:     "test-key",
		SecretAccessKey: "test-secret",
	}))

	rec2 := httptest.NewRecorder()
	handleConfig(rec2, req)

	var resp2 map[string]interface{}
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if resp2["s3_enabled"] != true {
		t.Errorf("expected s3_enabled to be true, got %v", resp2["s3_enabled"])
	}
	if resp2["s3_bucket"] != "test-bucket" {
		t.Errorf("expected s3_bucket 'test-bucket', got %v", resp2["s3_bucket"])
	}
}

func TestHandleS3Presign(t *testing.T) {
	// S3 configured
	storage.SetDefaultClient(storage.NewClient(storage.Config{
		Bucket:          "upload-bucket",
		Region:          "us-east-1",
		AccessKeyID:     "key123",
		SecretAccessKey: "secret123",
		ForcePathStyle:  true,
		UseSSL:          true,
	}))

	payload := `{"filename": "math_primary.pdf", "content_type": "application/pdf"}`
	req := httptest.NewRequest(http.MethodPost, "/api/s3/presign", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handleS3Presign(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse json response: %v", err)
	}

	if resp["success"] != true {
		t.Errorf("expected success true, got %v", resp["success"])
	}
	key, _ := resp["key"].(string)
	if !strings.HasPrefix(key, "sessions/") || !strings.HasSuffix(key, "/math_primary.pdf") {
		t.Errorf("unexpected key format: %s", key)
	}
	uploadURL, _ := resp["upload_url"].(string)
	if !strings.Contains(uploadURL, "X-Amz-Signature=") {
		t.Errorf("presigned upload URL missing signature: %s", uploadURL)
	}
}

func TestHandleClear(t *testing.T) {
	tmpDir := t.TempDir()
	activeConfig = ServerConfig{
		SessionsDir: tmpDir,
	}

	sessionID := "test_session_12345"
	sessionPath := filepath.Join(tmpDir, sessionID)
	_ = os.MkdirAll(filepath.Join(sessionPath, "input"), 0755)

	// Create test dummy file
	dummyFile := filepath.Join(sessionPath, "input", "dummy.pdf")
	_ = os.WriteFile(dummyFile, []byte("dummy"), 0644)

	// Invoke /api/clear with JSON body
	payload := `{"session_id": "test_session_12345"}`
	req := httptest.NewRequest(http.MethodPost, "/api/clear", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handleClear(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify local session folder is gone
	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Errorf("expected session directory %s to be deleted", sessionPath)
	}
}

func TestHandleDownloadApp(t *testing.T) {
	// Test platform=windows
	reqWin := httptest.NewRequest(http.MethodGet, "/api/download-app?platform=windows", nil)
	recWin := httptest.NewRecorder()
	handleDownloadApp(recWin, reqWin)

	// In test environment, pdf-plitter.exe exists in root or returns file
	if recWin.Code == http.StatusOK {
		cd := recWin.Header().Get("Content-Disposition")
		if !strings.Contains(cd, "pdf-plitter.exe") {
			t.Errorf("expected Content-Disposition to contain pdf-plitter.exe, got: %s", cd)
		}
	}

	// Test platform=darwin
	reqMac := httptest.NewRequest(http.MethodGet, "/api/download-app?platform=darwin", nil)
	recMac := httptest.NewRecorder()
	handleDownloadApp(recMac, reqMac)
	if recMac.Code != http.StatusNotFound {
		t.Errorf("expected 404 for macOS, got %d", recMac.Code)
	}
}

func TestHandleHeartbeat(t *testing.T) {
	activeConfig = ServerConfig{
		Standalone: true,
	}

	req := httptest.NewRequest(http.MethodPost, "/api/heartbeat", nil)
	rec := httptest.NewRecorder()
	handleHeartbeat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode json: %v", err)
	}

	if resp["status"] != "ok" || resp["standalone"] != true {
		t.Errorf("unexpected heartbeat response: %v", resp)
	}
}

func TestHandleShutdown(t *testing.T) {
	tmpDir := t.TempDir()
	activeConfig = ServerConfig{
		SessionsDir: tmpDir,
		Standalone:  false, // Non-standalone: verify it clears session without killing test process
	}

	sessionID := "test_session_shutdown"
	sessionPath := filepath.Join(tmpDir, sessionID)
	_ = os.MkdirAll(filepath.Join(sessionPath, "input"), 0755)

	payload := `{"session_id": "test_session_shutdown"}`
	req := httptest.NewRequest(http.MethodPost, "/api/shutdown", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handleShutdown(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Errorf("expected session directory %s to be purged on shutdown call", sessionPath)
	}

	// Test shutdown with empty session_id (e.g. browser closed before selecting book)
	reqEmpty := httptest.NewRequest(http.MethodPost, "/api/shutdown", strings.NewReader(`{}`))
	reqEmpty.Header.Set("Content-Type", "application/json")
	recEmpty := httptest.NewRecorder()
	handleShutdown(recEmpty, reqEmpty)
	if recEmpty.Code != http.StatusOK {
		t.Fatalf("expected status 200 for empty session shutdown, got %d: %s", recEmpty.Code, recEmpty.Body.String())
	}
}
