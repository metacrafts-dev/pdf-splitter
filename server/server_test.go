package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pdf-splitter/storage"
)

func buildMinimalPDFBytes(pages int) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, 0, pages+2)

	offsets = append(offsets, buf.Len())
	buf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")

	var kids strings.Builder
	for i := 0; i < pages; i++ {
		if i > 0 {
			kids.WriteByte(' ')
		}
		fmt.Fprintf(&kids, "%d 0 R", i+3)
	}
	offsets = append(offsets, buf.Len())
	fmt.Fprintf(&buf, "2 0 obj\n<< /Type /Pages /Kids [%s] /Count %d >>\nendobj\n", kids.String(), pages)

	for i := 0; i < pages; i++ {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << >> >>\nendobj\n", i+3)
	}

	xrefOffset := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(offsets)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xrefOffset)
	return buf.Bytes()
}

func newTestServer(t *testing.T, standalone bool, store *storage.Client) *Server {
	t.Helper()
	if store == nil {
		store = storage.NewClient(storage.Config{})
	}
	s, err := NewServer(ServerConfig{
		SessionsDir:    t.TempDir(),
		Retention:      15 * time.Minute,
		MaxUploadBytes: 10 << 20,
		Standalone:     standalone,
		Headless:       true,
	}, store)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	return s
}

func TestRecoveryMiddlewareReturnsJSON500(t *testing.T) {
	handler := recoveryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom: simulated handler crash")
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/inspect", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 after panic, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("expected application/json Content-Type, got %q", ct)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected valid JSON body, got %q: %v", rec.Body.String(), err)
	}
	if resp["error"] == "" {
		t.Errorf("expected non-empty error message in JSON response")
	}
}

func TestNewServerInvalidSessionsDirMentionsEnvVar(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := NewServer(ServerConfig{
		SessionsDir: filepath.Join(blocker, "sessions"),
	}, storage.NewClient(storage.Config{}))
	if err == nil {
		t.Fatalf("expected NewServer to fail when SessionsDir cannot be created")
	}
	if !strings.Contains(err.Error(), "SESSIONS_DIR") {
		t.Errorf("expected error to mention SESSIONS_DIR, got: %v", err)
	}
}

func TestHandleConfigDoesNotLeakS3Bucket(t *testing.T) {
	store := storage.NewClient(storage.Config{
		Bucket:          "secret-internal-bucket",
		Region:          "eu-central-1",
		AccessKeyID:     "test-key",
		SecretAccessKey: "test-secret",
	})
	s := newTestServer(t, false, store)

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if resp["s3_enabled"] != true {
		t.Errorf("expected s3_enabled=true, got %v", resp["s3_enabled"])
	}
	if _, leaked := resp["s3_bucket"]; leaked {
		t.Errorf("s3_bucket should not be exposed in /api/config")
	}
	if _, leaked := resp["s3_region"]; leaked {
		t.Errorf("s3_region should not be exposed in /api/config")
	}
}

func TestHealthzAndSecurityHeaders(t *testing.T) {
	s := newTestServer(t, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /healthz, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "script-src 'self'") {
		t.Errorf("expected strict CSP header, got %q", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("expected X-Frame-Options DENY, got %q", got)
	}
}

func TestIndexIncludesCancelAndTelemetryControls(t *testing.T) {
	s := newTestServer(t, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, needle := range []string{`id="cancelUploadBtn"`, `id="progressSpeedText"`, `id="progressEtaText"`, s.CSRFToken()} {
		if !strings.Contains(body, needle) {
			t.Errorf("expected index HTML to contain %q", needle)
		}
	}
}

func TestInspectCancelledContextCleansUpSession(t *testing.T) {
	s := newTestServer(t, false, nil)
	pdfBytes := buildMinimalPDFBytes(3)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "cancel_test.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(fw, bytes.NewReader(pdfBytes)); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodPost, "/api/inspect", &body).WithContext(ctx)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", s.CSRFToken())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("expected cancelled inspect request to fail, got 200")
	}

	entries, err := os.ReadDir(s.cfg.SessionsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no leftover session dirs after cancelled inspect, found %d", len(entries))
	}
}

func TestDesktopEndpointsDisabledInVPSMode(t *testing.T) {
	s := newTestServer(t, false, nil)

	for _, path := range []string{"/api/open-folder", "/api/shutdown", "/api/heartbeat", "/api/events"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", s.CSRFToken())
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404 for %s in VPS mode, got %d", path, rec.Code)
		}
	}
}

func TestCSRFProtectionEnforced(t *testing.T) {
	s := newTestServer(t, false, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/clear", strings.NewReader(`{"session_id":"0123456789abcdef0123456789abcdef"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 without CSRF token, got %d", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/api/clear", strings.NewReader(`{"session_id":"0123456789abcdef0123456789abcdef"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-CSRF-Token", s.CSRFToken())
	req2.Header.Set("Sec-Fetch-Site", "cross-site")
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("expected 403 for cross-site Sec-Fetch-Site, got %d", rec2.Code)
	}
}

func TestClearRejectsGETAndPathTraversal(t *testing.T) {
	s := newTestServer(t, false, nil)

	reqGet := httptest.NewRequest(http.MethodGet, "/api/clear?session_id=0123456789abcdef0123456789abcdef&csrf_token="+s.CSRFToken(), nil)
	recGet := httptest.NewRecorder()
	s.Handler().ServeHTTP(recGet, reqGet)
	if recGet.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET /api/clear, got %d", recGet.Code)
	}

	sentinel := filepath.Join(filepath.Dir(s.cfg.SessionsDir), "keep_me.txt")
	if err := os.WriteFile(sentinel, []byte("precious"), 0644); err != nil {
		t.Fatal(err)
	}

	for _, badID := range []string{"..", ".", "../etc", "abc/def", "not-32-hex-chars"} {
		payload := fmt.Sprintf(`{"session_id":%q}`, badID)
		req := httptest.NewRequest(http.MethodPost, "/api/clear", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", s.CSRFToken())
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for bad session_id %q, got %d", badID, rec.Code)
		}
	}

	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel file outside SessionsDir was touched: %v", err)
	}
}

func TestS3PresignAndKeyAuthorization(t *testing.T) {
	store := storage.NewClient(storage.Config{
		Bucket:          "upload-bucket",
		Region:          "us-east-1",
		AccessKeyID:     "key123",
		SecretAccessKey: "secret123",
		ForcePathStyle:  true,
		UseSSL:          true,
	})
	s := newTestServer(t, false, store)

	bigPayload := fmt.Sprintf(`{"filename":"huge.pdf","file_size":%d}`, s.cfg.MaxUploadBytes+1)
	reqBig := httptest.NewRequest(http.MethodPost, "/api/s3/presign", strings.NewReader(bigPayload))
	reqBig.Header.Set("Content-Type", "application/json")
	reqBig.Header.Set("X-CSRF-Token", s.CSRFToken())
	recBig := httptest.NewRecorder()
	s.Handler().ServeHTTP(recBig, reqBig)
	if recBig.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized presign, got %d", recBig.Code)
	}

	validPayload := `{"filename":"math_primary.pdf","file_size":4096}`
	req := httptest.NewRequest(http.MethodPost, "/api/s3/presign", strings.NewReader(validPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", s.CSRFToken())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	sid, _ := resp["session_id"].(string)
	if !isValidSessionID(sid) {
		t.Fatalf("expected 32-char hex session_id, got %q", sid)
	}
	uploadURL, _ := resp["upload_url"].(string)
	if !strings.Contains(uploadURL, "content-length") || !strings.Contains(uploadURL, "content-type") {
		t.Errorf("expected presigned URL to bind content-length and content-type: %s", uploadURL)
	}

	hijackPayload := fmt.Sprintf(`{"session_id":%q,"s3_key":"sessions/other/input/secret.pdf"}`, sid)
	reqHijack := httptest.NewRequest(http.MethodPost, "/api/inspect", strings.NewReader(hijackPayload))
	reqHijack.Header.Set("Content-Type", "application/json")
	reqHijack.Header.Set("X-CSRF-Token", s.CSRFToken())
	recHijack := httptest.NewRecorder()
	s.Handler().ServeHTTP(recHijack, reqHijack)
	if recHijack.Code != http.StatusForbidden {
		t.Errorf("expected 403 for unauthorized S3 key hijack, got %d: %s", recHijack.Code, recHijack.Body.String())
	}
}

func TestEndToEndInspectSplitDownloadArabicFilename(t *testing.T) {
	s := newTestServer(t, false, nil)
	pdfBytes := buildMinimalPDFBytes(6)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "كتاب_العلوم.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(fw, bytes.NewReader(pdfBytes)); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	reqInspect := httptest.NewRequest(http.MethodPost, "/api/inspect", &body)
	reqInspect.Header.Set("Content-Type", mw.FormDataContentType())
	reqInspect.Header.Set("X-CSRF-Token", s.CSRFToken())
	recInspect := httptest.NewRecorder()
	s.Handler().ServeHTTP(recInspect, reqInspect)

	if recInspect.Code != http.StatusOK {
		t.Fatalf("inspect failed (%d): %s", recInspect.Code, recInspect.Body.String())
	}

	var inspectResp map[string]any
	if err := json.Unmarshal(recInspect.Body.Bytes(), &inspectResp); err != nil {
		t.Fatal(err)
	}
	sid, _ := inspectResp["session_id"].(string)
	if !isValidSessionID(sid) {
		t.Fatalf("invalid session_id returned: %q", sid)
	}
	if _, leaked := inspectResp["path"]; leaked {
		t.Errorf("BookInfo.Path must not be serialized in /api/inspect response")
	}

	splitPayload := fmt.Sprintf(`{"session_id":%q,"split_points":"2, 4"}`, sid)
	reqSplit := httptest.NewRequest(http.MethodPost, "/api/split", strings.NewReader(splitPayload))
	reqSplit.Header.Set("Content-Type", "application/json")
	reqSplit.Header.Set("X-CSRF-Token", s.CSRFToken())
	recSplit := httptest.NewRecorder()
	s.Handler().ServeHTTP(recSplit, reqSplit)

	if recSplit.Code != http.StatusOK {
		t.Fatalf("split failed (%d): %s", recSplit.Code, recSplit.Body.String())
	}

	var splitResp struct {
		Success        bool   `json:"success"`
		OutputDir      string `json:"output_dir"`
		ZipDownloadURL string `json:"zip_download_url"`
		Parts          []struct {
			FileName    string `json:"file_name"`
			FilePath    string `json:"file_path"`
			DownloadURL string `json:"download_url"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(recSplit.Body.Bytes(), &splitResp); err != nil {
		t.Fatal(err)
	}
	if !splitResp.Success || len(splitResp.Parts) != 3 {
		t.Fatalf("unexpected split response: %+v", splitResp)
	}
	if splitResp.OutputDir != "" {
		t.Errorf("raw server output_dir should not be returned")
	}
	for _, p := range splitResp.Parts {
		if p.FilePath != "" {
			t.Errorf("part file_path should be scrubbed, got %q", p.FilePath)
		}
	}

	reqZip := httptest.NewRequest(http.MethodGet, splitResp.ZipDownloadURL, nil)
	recZip := httptest.NewRecorder()
	s.Handler().ServeHTTP(recZip, reqZip)
	if recZip.Code != http.StatusOK {
		t.Fatalf("zip download failed (%d)", recZip.Code)
	}
	cd := recZip.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("expected RFC-encoded attachment Content-Disposition, got %q", cd)
	}

	reqClear := httptest.NewRequest(http.MethodDelete, "/api/clear?session_id="+sid, nil)
	reqClear.Header.Set("X-CSRF-Token", s.CSRFToken())
	recClear := httptest.NewRecorder()
	s.Handler().ServeHTTP(recClear, reqClear)
	if recClear.Code != http.StatusOK {
		t.Fatalf("clear failed (%d): %s", recClear.Code, recClear.Body.String())
	}

	sessionPath := filepath.Join(s.cfg.SessionsDir, sid)
	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Errorf("expected session directory to be deleted after /api/clear")
	}
}
