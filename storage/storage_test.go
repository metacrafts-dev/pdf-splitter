package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigIsConfigured(t *testing.T) {
	cfg := Config{}
	if cfg.IsConfigured() {
		t.Errorf("expected empty config to not be configured")
	}

	cfg = Config{
		Bucket:          "my-bucket",
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}
	if !cfg.IsConfigured() {
		t.Errorf("expected config with bucket, key, secret to be configured")
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	t.Setenv("S3_BUCKET", "env-bucket")
	t.Setenv("S3_REGION", "eu-central-1")
	t.Setenv("S3_ACCESS_KEY_ID", "env-key")
	t.Setenv("S3_SECRET_ACCESS_KEY", "env-secret")
	t.Setenv("S3_FORCE_PATH_STYLE", "true")
	t.Setenv("S3_USE_SSL", "false")
	t.Setenv("S3_ENDPOINT", "http://localhost:9000")

	cfg := LoadConfigFromEnv()
	if cfg.Bucket != "env-bucket" {
		t.Errorf("expected bucket 'env-bucket', got '%s'", cfg.Bucket)
	}
	if cfg.Region != "eu-central-1" {
		t.Errorf("expected region 'eu-central-1', got '%s'", cfg.Region)
	}
	if !cfg.ForcePathStyle {
		t.Errorf("expected ForcePathStyle to be true")
	}
	if cfg.UseSSL {
		t.Errorf("expected UseSSL to be false")
	}
	if !cfg.IsConfigured() {
		t.Errorf("expected LoadConfigFromEnv to be configured")
	}
}

func TestPresignedURLGeneration(t *testing.T) {
	cfg := Config{
		Bucket:          "test-bucket",
		Region:          "us-east-1",
		AccessKeyID:     "test-key",
		SecretAccessKey: "test-secret",
		Endpoint:        "s3.amazonaws.com",
		ForcePathStyle:  true,
		UseSSL:          true,
	}

	client := NewClient(cfg)
	ctx := context.Background()

	putURL, err := client.GeneratePresignedPutURL(ctx, "sessions/123/input/test book.pdf", 15*time.Minute)
	if err != nil {
		t.Fatalf("GeneratePresignedPutURL failed: %v", err)
	}

	u, err := url.Parse(putURL)
	if err != nil {
		t.Fatalf("failed to parse generated URL: %v", err)
	}

	q := u.Query()
	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		t.Errorf("missing or incorrect X-Amz-Algorithm: %s", q.Get("X-Amz-Algorithm"))
	}
	if !strings.Contains(q.Get("X-Amz-Credential"), "test-key") {
		t.Errorf("credential does not contain access key: %s", q.Get("X-Amz-Credential"))
	}
	if q.Get("X-Amz-Expires") != "900" {
		t.Errorf("expected expires 900, got %s", q.Get("X-Amz-Expires"))
	}
	if q.Get("X-Amz-Signature") == "" {
		t.Errorf("expected non-empty signature")
	}
	if !strings.Contains(u.Path, "test-bucket") {
		t.Errorf("expected path to contain bucket name under path-style: %s", u.Path)
	}

	getURL, err := client.GeneratePresignedGetURL(ctx, "sessions/123/parts/part_01.pdf", 15*time.Minute)
	if err != nil {
		t.Fatalf("GeneratePresignedGetURL failed: %v", err)
	}
	if !strings.Contains(getURL, "X-Amz-Signature=") {
		t.Fatalf("expected presigned GET URL to contain signature")
	}
}

func TestMockS3Operations(t *testing.T) {
	storedObjects := make(map[string][]byte)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/test-bucket/")

		switch r.Method {
		case http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			storedObjects[key] = data
			w.WriteHeader(http.StatusOK)

		case http.MethodGet:
			if r.URL.Query().Get("list-type") == "2" {
				prefix := r.URL.Query().Get("prefix")
				w.Header().Set("Content-Type", "application/xml")
				fmt.Fprint(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
				fmt.Fprint(w, `<Name>test-bucket</Name><Prefix>`+prefix+`</Prefix><KeyCount>1</KeyCount>`)
				for k, v := range storedObjects {
					if strings.HasPrefix(k, prefix) {
						fmt.Fprintf(w, `<Contents><Key>%s</Key><LastModified>2026-11-20T20:00:00.000Z</LastModified><Size>%d</Size></Contents>`, k, len(v))
					}
				}
				fmt.Fprint(w, `</ListBucketResult>`)
				return
			}

			data, exists := storedObjects[key]
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)

		case http.MethodDelete:
			delete(storedObjects, key)
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	cfg := Config{
		Bucket:          "test-bucket",
		Region:          "us-east-1",
		AccessKeyID:     "mock-key",
		SecretAccessKey: "mock-secret",
		Endpoint:        server.URL,
		ForcePathStyle:  true,
		UseSSL:          false,
	}

	client := NewClient(cfg)
	ctx := context.Background()

	// 1. Upload
	testData := []byte("PDF mock content for testing")
	err := client.UploadObject(ctx, "sessions/abc/input/test.pdf", bytes.NewReader(testData), int64(len(testData)), "application/pdf")
	if err != nil {
		t.Fatalf("UploadObject failed: %v", err)
	}

	// 2. Download
	tmpDir := t.TempDir()
	downloadFile := filepath.Join(tmpDir, "downloaded.pdf")
	err = client.DownloadObject(ctx, "sessions/abc/input/test.pdf", downloadFile)
	if err != nil {
		t.Fatalf("DownloadObject failed: %v", err)
	}

	downloadedBytes, err := os.ReadFile(downloadFile)
	if err != nil {
		t.Fatalf("failed to read downloaded file: %v", err)
	}
	if string(downloadedBytes) != string(testData) {
		t.Fatalf("expected '%s', got '%s'", string(testData), string(downloadedBytes))
	}

	// 3. List
	objs, err := client.ListObjects(ctx, "sessions/abc/")
	if err != nil {
		t.Fatalf("ListObjects failed: %v", err)
	}
	if len(objs) != 1 || objs[0].Key != "sessions/abc/input/test.pdf" {
		t.Fatalf("unexpected list results: %+v", objs)
	}

	// 4. Delete Prefix
	deleted, err := client.DeletePrefix(ctx, "sessions/abc/")
	if err != nil {
		t.Fatalf("DeletePrefix failed: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 deleted, got %d", deleted)
	}

	// Verify deleted
	objsAfter, err := client.ListObjects(ctx, "sessions/abc/")
	if err != nil {
		t.Fatalf("ListObjects after delete failed: %v", err)
	}
	if len(objsAfter) != 0 {
		t.Fatalf("expected 0 objects, got %d", len(objsAfter))
	}
}
