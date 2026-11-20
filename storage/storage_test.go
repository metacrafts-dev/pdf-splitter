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

func TestResolveEndpointNormalizesPortsAndTrailingSlash(t *testing.T) {
	c := NewClient(Config{
		Bucket:         "mybucket",
		Endpoint:       "https://r2.example.com:443/",
		ForcePathStyle: false,
		UseSSL:         true,
	})
	scheme, host, basePath := c.resolveEndpoint()
	if scheme != "https" || host != "mybucket.r2.example.com" || basePath != "" {
		t.Errorf("unexpected endpoint resolution: scheme=%q host=%q basePath=%q", scheme, host, basePath)
	}
}

// TestSigV4OfficialVector verifies query-string presigned URL generation against
// the official AWS SigV4 GetObject presigned URL test vector.
func TestSigV4OfficialVector(t *testing.T) {
	client := NewClient(Config{
		Bucket:          "examplebucket",
		Region:          "us-east-1",
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		ForcePathStyle:  false,
		UseSSL:          true,
	})
	client.nowFunc = func() time.Time {
		return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	}

	got, err := client.GeneratePresignedGetURL(context.Background(), "test.txt", 86400*time.Second)
	if err != nil {
		t.Fatalf("GeneratePresignedGetURL failed: %v", err)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("url.Parse failed: %v", err)
	}
	const wantSig = "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if gotSig := u.Query().Get("X-Amz-Signature"); gotSig != wantSig {
		t.Errorf("SigV4 signature mismatch:\n got: %s\nwant: %s\n url: %s", gotSig, wantSig, got)
	}
}

func TestPresignedPutConstraintsAndDownloadDisposition(t *testing.T) {
	client := NewClient(Config{
		Bucket:          "test-bucket",
		Region:          "us-east-1",
		AccessKeyID:     "test-key",
		SecretAccessKey: "test-secret",
		Endpoint:        "s3.amazonaws.com",
		ForcePathStyle:  true,
		UseSSL:          true,
	})

	putURL, err := client.GeneratePresignedPutURLWithConstraints(
		context.Background(),
		"sessions/123/input/test book.pdf",
		15*time.Minute,
		1048576,
		"application/pdf",
	)
	if err != nil {
		t.Fatalf("GeneratePresignedPutURLWithConstraints failed: %v", err)
	}

	u, err := url.Parse(putURL)
	if err != nil {
		t.Fatalf("url.Parse failed: %v", err)
	}
	if got := u.Query().Get("X-Amz-SignedHeaders"); got != "content-length;content-type;host" {
		t.Errorf("expected sorted signed headers 'content-length;content-type;host', got %q", got)
	}

	dlURL, err := client.GeneratePresignedDownloadURL(
		context.Background(),
		"sessions/123/parts/part_01.pdf",
		"كتاب_العلوم.pdf",
		15*time.Minute,
	)
	if err != nil {
		t.Fatalf("GeneratePresignedDownloadURL failed: %v", err)
	}
	du, _ := url.Parse(dlURL)
	if !strings.Contains(du.Query().Get("response-content-disposition"), "attachment") {
		t.Errorf("expected attachment response-content-disposition, got %q", du.Query().Get("response-content-disposition"))
	}
}

func TestMockS3OperationsAndPagination(t *testing.T) {
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
				token := r.URL.Query().Get("continuation-token")
				w.Header().Set("Content-Type", "application/xml")

				// Simulate 2-page pagination for pagination test prefix
				if prefix == "paged/" {
					if token == "" {
						fmt.Fprint(w, `<ListBucketResult><Name>test-bucket</Name><Prefix>paged/</Prefix><IsTruncated>true</IsTruncated><NextContinuationToken>tok2</NextContinuationToken><Contents><Key>paged/a.pdf</Key><LastModified>2026-11-20T20:00:00Z</LastModified><Size>10</Size></Contents></ListBucketResult>`)
					} else {
						fmt.Fprint(w, `<ListBucketResult><Name>test-bucket</Name><Prefix>paged/</Prefix><IsTruncated>false</IsTruncated><Contents><Key>paged/b.pdf</Key><LastModified>2026-11-20T20:00:00Z</LastModified><Size>20</Size></Contents></ListBucketResult>`)
					}
					return
				}

				fmt.Fprint(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
				fmt.Fprint(w, `<Name>test-bucket</Name><Prefix>`+prefix+`</Prefix><IsTruncated>false</IsTruncated>`)
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

	client := NewClient(Config{
		Bucket:          "test-bucket",
		Region:          "us-east-1",
		AccessKeyID:     "mock-key",
		SecretAccessKey: "mock-secret",
		Endpoint:        server.URL,
		ForcePathStyle:  true,
		UseSSL:          false,
	})
	ctx := context.Background()

	// Verify pagination collects both pages
	pagedObjs, err := client.ListObjects(ctx, "paged/")
	if err != nil {
		t.Fatalf("paginated ListObjects failed: %v", err)
	}
	if len(pagedObjs) != 2 || pagedObjs[0].Key != "paged/a.pdf" || pagedObjs[1].Key != "paged/b.pdf" {
		t.Fatalf("unexpected paginated objects: %+v", pagedObjs)
	}

	// Upload / Download / Limit / Delete
	testData := []byte("PDF mock content for testing")
	if err := client.UploadObject(ctx, "sessions/abc/input/test.pdf", bytes.NewReader(testData), int64(len(testData)), "application/pdf"); err != nil {
		t.Fatalf("UploadObject failed: %v", err)
	}

	tmpDir := t.TempDir()
	// DownloadObjectWithLimit rejects oversized object
	if err := client.DownloadObjectWithLimit(ctx, "sessions/abc/input/test.pdf", filepath.Join(tmpDir, "too_big.pdf"), 5); err == nil {
		t.Errorf("expected DownloadObjectWithLimit to fail when object exceeds maxBytes")
	}

	downloadFile := filepath.Join(tmpDir, "downloaded.pdf")
	if err := client.DownloadObject(ctx, "sessions/abc/input/test.pdf", downloadFile); err != nil {
		t.Fatalf("DownloadObject failed: %v", err)
	}
	gotBytes, err := os.ReadFile(downloadFile)
	if err != nil || string(gotBytes) != string(testData) {
		t.Fatalf("downloaded content mismatch: %q, err=%v", string(gotBytes), err)
	}

	deleted, err := client.DeletePrefix(ctx, "sessions/abc/")
	if err != nil || deleted != 1 {
		t.Fatalf("DeletePrefix: deleted=%d, err=%v", deleted, err)
	}
}
