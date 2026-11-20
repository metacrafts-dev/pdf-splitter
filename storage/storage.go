// Package storage provides pure-Go AWS S3 and S3-compatible cloud object storage integration.
// It implements AWS Signature Version 4 (SigV4) for presigned PUT/GET URLs, object streaming,
// object listing, deletion, and lifecycle cleanup without external AWS SDK or CGo dependencies.
// Supported backends: AWS S3, Cloudflare R2, MinIO, Wasabi, Ceph, and LocalStack.
package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config holds S3 credential and connection parameters.
type Config struct {
	Bucket          string `json:"bucket"`
	Region          string `json:"region"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"-"` // Never expose secret key in JSON
	Endpoint        string `json:"endpoint,omitempty"`
	ForcePathStyle  bool   `json:"force_path_style"`
	UseSSL          bool   `json:"use_ssl"`
}

// IsConfigured returns true if the required S3 settings (Bucket, AccessKeyID, SecretAccessKey) are non-empty.
func (c Config) IsConfigured() bool {
	return strings.TrimSpace(c.Bucket) != "" &&
		strings.TrimSpace(c.AccessKeyID) != "" &&
		strings.TrimSpace(c.SecretAccessKey) != ""
}

// LoadConfigFromEnv inspects environment variables and returns a populated Config.
// Environment variables:
//   - S3_BUCKET: Target S3 bucket name (required for S3 mode)
//   - S3_REGION: AWS Region (default: "us-east-1")
//   - S3_ACCESS_KEY_ID: S3 access key / credential ID
//   - S3_SECRET_ACCESS_KEY: S3 secret key
//   - S3_ENDPOINT: Custom S3 endpoint URL (optional for MinIO, Cloudflare R2, Wasabi, etc.)
//   - S3_FORCE_PATH_STYLE: Set to "true" or "1" to force path-style access (e.g. for MinIO / LocalStack)
//   - S3_USE_SSL: Set to "false" or "0" to disable HTTPS (e.g. for local test MinIO servers)
func LoadConfigFromEnv() Config {
	region := os.Getenv("S3_REGION")
	if strings.TrimSpace(region) == "" {
		region = "us-east-1"
	}

	useSSL := true
	if sslVal := strings.ToLower(strings.TrimSpace(os.Getenv("S3_USE_SSL"))); sslVal != "" {
		if sslVal == "false" || sslVal == "0" || sslVal == "no" || sslVal == "off" {
			useSSL = false
		}
	}

	forcePathStyle := false
	if fpsVal := strings.ToLower(strings.TrimSpace(os.Getenv("S3_FORCE_PATH_STYLE"))); fpsVal != "" {
		if fpsVal == "true" || fpsVal == "1" || fpsVal == "yes" || fpsVal == "on" {
			forcePathStyle = true
		}
	}

	endpoint := strings.TrimSpace(os.Getenv("S3_ENDPOINT"))

	// Auto-enable path-style if endpoint points to localhost or an IP address without subdomain support
	if !forcePathStyle && endpoint != "" {
		lowerEP := strings.ToLower(endpoint)
		if strings.Contains(lowerEP, "localhost") || strings.Contains(lowerEP, "127.0.0.1") || strings.Contains(lowerEP, "0.0.0.0") {
			forcePathStyle = true
		}
	}

	return Config{
		Bucket:          strings.TrimSpace(os.Getenv("S3_BUCKET")),
		Region:          strings.TrimSpace(region),
		AccessKeyID:     strings.TrimSpace(os.Getenv("S3_ACCESS_KEY_ID")),
		SecretAccessKey: strings.TrimSpace(os.Getenv("S3_SECRET_ACCESS_KEY")),
		Endpoint:        endpoint,
		ForcePathStyle:  forcePathStyle,
		UseSSL:          useSSL,
	}
}

// S3Object represents a stored item in an S3 bucket.
type S3Object struct {
	Key          string    `xml:"Key"`
	LastModified time.Time `xml:"LastModified"`
	Size         int64     `xml:"Size"`
	ETag         string    `xml:"ETag"`
}

// listBucketResult maps AWS S3 ListObjectsV2 XML response.
type listBucketResult struct {
	XMLName     xml.Name   `xml:"ListBucketResult"`
	Name        string     `xml:"Name"`
	Prefix      string     `xml:"Prefix"`
	KeyCount    int        `xml:"KeyCount"`
	MaxKeys     int        `xml:"MaxKeys"`
	IsTruncated bool       `xml:"IsTruncated"`
	Contents    []S3Object `xml:"Contents"`
}

// Client manages pure Go S3 operations using AWS SigV4.
type Client struct {
	cfg        Config
	httpClient *http.Client
}

// Global default client singleton
var (
	defaultMu     sync.RWMutex
	defaultClient *Client
)

func init() {
	SetDefaultClient(NewClient(LoadConfigFromEnv()))
}

// NewClient initializes a new storage Client with the specified configuration.
func NewClient(cfg Config) *Client {
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 30 * time.Minute,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// SetDefaultClient updates the global storage client.
func SetDefaultClient(c *Client) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultClient = c
}

// DefaultClient returns the current global storage client.
func DefaultClient() *Client {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultClient
}

// IsConfigured returns true if the global default storage client is configured with S3 credentials.
func IsConfigured() bool {
	c := DefaultClient()
	return c != nil && c.IsConfigured()
}

// Config returns the configuration of the client.
func (c *Client) Config() Config {
	return c.cfg
}

// IsConfigured returns true if the client has valid S3 credentials.
func (c *Client) IsConfigured() bool {
	return c != nil && c.cfg.IsConfigured()
}

// resolveEndpoint resolves the URL scheme, host header, and base path for S3 requests.
func (c *Client) resolveEndpoint() (scheme, host, basePath string) {
	scheme = "https"
	if !c.cfg.UseSSL {
		scheme = "http"
	}

	ep := strings.TrimSpace(c.cfg.Endpoint)
	if ep != "" {
		if strings.HasPrefix(ep, "http://") || strings.HasPrefix(ep, "https://") {
			if u, err := url.Parse(ep); err == nil {
				scheme = u.Scheme
				basePath = strings.TrimSuffix(u.Path, "/")
				if c.cfg.ForcePathStyle {
					return scheme, u.Host, basePath
				}
				return scheme, fmt.Sprintf("%s.%s", c.cfg.Bucket, u.Host), basePath
			}
		}
		// Endpoint provided as hostname (without scheme)
		if c.cfg.ForcePathStyle {
			return scheme, ep, ""
		}
		return scheme, fmt.Sprintf("%s.%s", c.cfg.Bucket, ep), ""
	}

	// Default standard AWS S3 endpoint
	var s3Host string
	if c.cfg.Region == "" || c.cfg.Region == "us-east-1" {
		s3Host = "s3.amazonaws.com"
	} else {
		s3Host = fmt.Sprintf("s3.%s.amazonaws.com", c.cfg.Region)
	}

	if c.cfg.ForcePathStyle {
		return scheme, s3Host, ""
	}
	return scheme, fmt.Sprintf("%s.%s", c.cfg.Bucket, s3Host), ""
}

// uriEncode implements RFC 3986 URI encoding for AWS SigV4.
// If encodeSlash is false, '/' characters are preserved (for canonical URI paths).
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') ||
			ch == '_' || ch == '-' || ch == '~' || ch == '.' {
			b.WriteByte(ch)
		} else if ch == '/' && !encodeSlash {
			b.WriteByte(ch)
		} else {
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// getSignatureKey derives the AWS SigV4 signing key.
func getSignatureKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}

// buildPresignedURL generates an AWS SigV4 query-authenticated presigned URL for the given HTTP method,
// optional key, and optional extra query parameters (e.g. for ListObjectsV2).
func (c *Client) buildPresignedURL(method, key string, extraParams [][2]string, expires time.Duration) (string, error) {
	if !c.IsConfigured() {
		return "", errors.New("S3 client is not configured")
	}

	now := time.Now().UTC()
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	expiresSec := int(expires.Seconds())
	if expiresSec <= 0 {
		expiresSec = 900 // Default to 15 minutes
	}
	if expiresSec > 604800 {
		expiresSec = 604800 // AWS maximum 7 days
	}

	scheme, host, basePath := c.resolveEndpoint()
	cleanKey := strings.TrimPrefix(key, "/")

	var canonicalPath string
	if c.cfg.ForcePathStyle {
		if cleanKey != "" {
			canonicalPath = basePath + "/" + c.cfg.Bucket + "/" + cleanKey
		} else {
			canonicalPath = basePath + "/" + c.cfg.Bucket
		}
	} else {
		if cleanKey != "" {
			canonicalPath = basePath + "/" + cleanKey
		} else {
			canonicalPath = basePath + "/"
		}
	}

	canonicalURI := uriEncode(canonicalPath, false)
	if !strings.HasPrefix(canonicalURI, "/") {
		canonicalURI = "/" + canonicalURI
	}

	credential := fmt.Sprintf("%s/%s/%s/s3/aws4_request", c.cfg.AccessKeyID, dateStamp, c.cfg.Region)

	// Base SigV4 Query parameters
	queryParams := [][2]string{
		{"X-Amz-Algorithm", "AWS4-HMAC-SHA256"},
		{"X-Amz-Credential", credential},
		{"X-Amz-Date", amzDate},
		{"X-Amz-Expires", strconv.Itoa(expiresSec)},
		{"X-Amz-SignedHeaders", "host"},
	}

	// Append any extra query parameters (e.g. list-type, prefix)
	queryParams = append(queryParams, extraParams...)

	// Sort query parameters in ascending byte order according to AWS SigV4 spec
	sort.Slice(queryParams, func(i, j int) bool {
		if queryParams[i][0] == queryParams[j][0] {
			return queryParams[i][1] < queryParams[j][1]
		}
		return queryParams[i][0] < queryParams[j][0]
	})

	var qParts []string
	for _, param := range queryParams {
		qParts = append(qParts, uriEncode(param[0], true)+"="+uriEncode(param[1], true))
	}
	canonicalQueryString := strings.Join(qParts, "&")

	canonicalHeaders := "host:" + host + "\n"
	signedHeaders := "host"
	payloadHash := "UNSIGNED-PAYLOAD"

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		canonicalQueryString,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	crHash := sha256Hex([]byte(canonicalRequest))

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, c.cfg.Region),
		crHash,
	}, "\n")

	kSigning := getSignatureKey(c.cfg.SecretAccessKey, dateStamp, c.cfg.Region, "s3")
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	finalURL := fmt.Sprintf("%s://%s%s?%s&X-Amz-Signature=%s", scheme, host, canonicalURI, canonicalQueryString, signature)
	return finalURL, nil
}

// GeneratePresignedPutURL generates a presigned PUT URL allowing clients (e.g. browser frontend)
// to upload an object directly to S3 with live upload progress.
func (c *Client) GeneratePresignedPutURL(ctx context.Context, key string, expires time.Duration) (string, error) {
	return c.buildPresignedURL(http.MethodPut, key, nil, expires)
}

// GeneratePresignedGetURL generates a presigned GET URL allowing clients to download an object
// directly from S3.
func (c *Client) GeneratePresignedGetURL(ctx context.Context, key string, expires time.Duration) (string, error) {
	return c.buildPresignedURL(http.MethodGet, key, nil, expires)
}

// UploadObject streams an object to S3 using the provided reader and size.
func (c *Client) UploadObject(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	putURL, err := c.GeneratePresignedPutURL(ctx, key, 2*time.Hour)
	if err != nil {
		return fmt.Errorf("failed to generate S3 presigned PUT URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, body)
	if err != nil {
		return fmt.Errorf("failed to create S3 PUT request: %w", err)
	}

	if size > 0 {
		req.ContentLength = size
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute S3 upload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("S3 upload returned HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	return nil
}

// UploadFile uploads a local file to S3 at the specified key.
func (c *Client) UploadFile(ctx context.Context, key string, localFilePath string, contentType string) error {
	fi, err := os.Stat(localFilePath)
	if err != nil {
		return fmt.Errorf("failed to stat file %s: %w", localFilePath, err)
	}

	f, err := os.Open(localFilePath)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", localFilePath, err)
	}
	defer f.Close()

	if contentType == "" {
		ext := strings.ToLower(filepath.Ext(localFilePath))
		switch ext {
		case ".pdf":
			contentType = "application/pdf"
		case ".zip":
			contentType = "application/zip"
		default:
			contentType = "application/octet-stream"
		}
	}

	return c.UploadObject(ctx, key, f, fi.Size(), contentType)
}

// DownloadObject streams an S3 object and writes it atomically to the specified local file path.
func (c *Client) DownloadObject(ctx context.Context, key string, destFilePath string) error {
	rc, _, err := c.GetObjectReader(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()

	if err := os.MkdirAll(filepath.Dir(destFilePath), 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	tmpPath := destFilePath + ".download.tmp"
	dst, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}

	if _, err := io.Copy(dst, rc); err != nil {
		dst.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to write downloaded stream: %w", err)
	}
	dst.Close()

	if err := os.Rename(tmpPath, destFilePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to finalize downloaded file: %w", err)
	}

	return nil
}

// GetObjectReader retrieves an S3 object as a readable stream along with its content length.
// The caller is responsible for closing the returned ReadCloser.
func (c *Client) GetObjectReader(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	getURL, err := c.GeneratePresignedGetURL(ctx, key, 2*time.Hour)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to generate S3 presigned GET URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, getURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create S3 GET request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to execute S3 GET: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, 0, fmt.Errorf("S3 GET returned HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	return resp.Body, resp.ContentLength, nil
}

// DeleteObject deletes an object from S3.
func (c *Client) DeleteObject(ctx context.Context, key string) error {
	delURL, err := c.buildPresignedURL(http.MethodDelete, key, nil, 15*time.Minute)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, delURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create S3 DELETE request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute S3 DELETE: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("S3 DELETE returned HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	return nil
}

// ListObjects queries S3 for objects matching a given key prefix using ListObjectsV2.
func (c *Client) ListObjects(ctx context.Context, prefix string) ([]S3Object, error) {
	if !c.IsConfigured() {
		return nil, errors.New("S3 client is not configured")
	}

	extraParams := [][2]string{
		{"list-type", "2"},
		{"max-keys", "1000"},
	}
	if prefix != "" {
		extraParams = append(extraParams, [2]string{"prefix", prefix})
	}

	listURL, err := c.buildPresignedURL(http.MethodGet, "", extraParams, 15*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("failed to construct S3 ListObjects URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create S3 ListObjects request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute S3 ListObjects: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("S3 ListObjects returned HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	var result listBucketResult
	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse S3 ListObjects XML response: %w", err)
	}

	return result.Contents, nil
}

// DeletePrefix deletes all objects under the specified prefix in S3.
func (c *Client) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if !c.IsConfigured() {
		return 0, nil
	}

	objects, err := c.ListObjects(ctx, prefix)
	if err != nil {
		return 0, err
	}

	deletedCount := 0
	for _, obj := range objects {
		if err := c.DeleteObject(ctx, obj.Key); err == nil {
			deletedCount++
		}
	}

	return deletedCount, nil
}

// PurgeStaleObjects finds and removes objects under prefix that are older than maxAge.
func (c *Client) PurgeStaleObjects(ctx context.Context, prefix string, maxAge time.Duration) (int, error) {
	if !c.IsConfigured() {
		return 0, nil
	}

	objects, err := c.ListObjects(ctx, prefix)
	if err != nil {
		return 0, err
	}

	now := time.Now()
	purgedCount := 0
	for _, obj := range objects {
		if !obj.LastModified.IsZero() && now.Sub(obj.LastModified) > maxAge {
			if err := c.DeleteObject(ctx, obj.Key); err == nil {
				purgedCount++
			}
		}
	}

	return purgedCount, nil
}
