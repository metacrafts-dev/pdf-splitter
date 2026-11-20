# PDF Splitter (Go) — Web UI, VPS Server & CLI

A compiled, standalone PDF splitter built in pure Go (`CGO_ENABLED=0`) with zero external runtime dependencies. It runs as an **offline CLI tool**, a **local desktop app**, or a **hardened VPS web service** with Store-mode ZIP packaging, S3-compatible object storage, and automatic local + S3 TTL cleanup.

---

## Key Features

### 1. Bilingual Web UI (Arabic Default with RTL & English)
- Starts in Arabic (`العربية`) with full RTL layout and one-click switching to English.
- Real-time upload progress bar with **live percentage, transferred MB, smoothed upload speed (B/s, KB/s, MB/s), estimated time remaining (ETA), and mid-upload Cancel button** (aborts in-flight XHR/fetch requests and cleans up pending S3 sessions).
- Inclusive split-point preview showing exact page ranges and the remainder block.
- Strict Content Security Policy (`script-src 'self'`), per-boot CSRF tokens, and zero inline scripts.

### 2. Fast Single-Pass Splitting, Store-Mode ZIP & Auto-Purge
- **Single-pass `pdfcpu.SplitByPageNr`**: parses the source PDF once rather than once per output part.
- **Uncompressed (`zip.Store`) atomic ZIP archives**: avoids redundant Deflate CPU burn on already-compressed PDFs and writes via temp-file + atomic rename.
- **Automatic Retention (default `15m`)**: background cleaner purges expired session directories, evicts in-memory session metadata, and runs periodic paginated S3 cleanup.

### 3. S3 Cloud Storage & Local Fallback
- Direct browser-to-S3 uploads via SigV4 presigned PUT URLs binding exact `Content-Length` and `Content-Type: application/pdf`.
- Supports AWS S3, Cloudflare R2, MinIO, Wasabi, and DigitalOcean Spaces.
- Server-minted session keys prevent unauthorized S3 key access.

### 4. Security Hardening
- Strict 32-char lowercase hex session ID validation and path containment across all endpoints.
- Desktop-only routes (`/api/events`, `/api/open-folder`, `/api/shutdown`, `/api/heartbeat`) are disabled in VPS (`-server`) mode.
- Hard `http.MaxBytesReader` upload limits (`MAX_UPLOAD_MB`), `32 MiB` multipart RAM cap, `%PDF-` header verification, concurrency semaphore, per-IP rate limiting, and `MaxSplitPoints` cap (`200`).

---

## Inclusive Splitting Logic

Split points mark the **inclusive end page** for each part:
- Split at `10, 20, 40` on a 100-page book:
  - **Part 1**: Pages 1–10 (10 pages)
  - **Part 2**: Pages 11–20 (10 pages)
  - **Part 3**: Pages 21–40 (20 pages)
  - **Part 4 (Rest)**: Pages 41–100 (60 pages)

---

## How to Run

### Mode A: Local Desktop (Windows / Linux / macOS)
Double-click `pdf-splitter.exe` or run without arguments:
```bash
./pdf-splitter
```
- Binds to `127.0.0.1:8080` and opens your default browser.
- Tracks active browser tabs over Server-Sent Events (`/api/events`) with a 15-second disconnect grace period so F5 reloads and background tab throttling do not terminate your session.

### Mode B: Command Line (CLI)
```bash
# Split and create a ZIP bundle
./pdf-splitter -input "book.pdf" -split "10,20,40" -zip

# Custom output directory
./pdf-splitter -input "book.pdf" -split "10,20,40" -output "./parts" -zip

# Positional syntax
./pdf-splitter "chemistry.pdf" 10 20 40 -zip
```

### Mode C: Production VPS Deployment
Deployment assets live in `deploy/`:
- [`deploy/deploy.sh`](deploy/deploy.sh): Idempotent Linux installer (Ubuntu/Debian/RHEL/Rocky/Alma).
- [`deploy/pdf-splitter.service`](deploy/pdf-splitter.service): Sandboxed systemd unit (`ProtectSystem=strict`, `NoNewPrivileges=true`, `MemoryDenyWriteExecute=true`).
- [`deploy/nginx.conf`](deploy/nginx.conf): Reverse proxy with streaming uploads, rate/connection limits, and TLS notes.
- [`.env.example`](.env.example): Configuration template.

```bash
./build.sh
sudo bash deploy/deploy.sh
```

> **Cloudflare Tunnel Note:** Cloudflare Free/Pro plans enforce a 100 MB HTTP request body limit. If exposing via `deploy/publish-cloudflare.sh` and users upload PDFs > 100 MB, configure S3/R2 credentials in `/opt/pdf-splitter/.env` so uploads bypass Cloudflare and go directly to S3.

---

## Environment Variables Reference

| Variable | Type | Default | Description |
|---|---|---|---|
| `PORT` | int | `8080` | TCP port to listen on |
| `HOST` | string | `127.0.0.1` | Host IP to bind (`127.0.0.1` behind Nginx/cloudflared) |
| `RETENTION` | duration | `15m` | Auto-purge TTL for local and S3 sessions |
| `MAX_UPLOAD_MB` | int | `500` | Maximum allowed PDF upload size in MB |
| `SESSIONS_DIR` | string | `./dist/data/sessions` | Session directory (`/opt/pdf-splitter/data/sessions` on VPS) |
| `HEADLESS` | bool | `false` (`true` in `.env.example`) | Skip launching a desktop browser |
| `S3_BUCKET` | string | `""` | S3 bucket name (empty = local storage mode) |
| `S3_REGION` | string | `us-east-1` | S3 region (`auto` for Cloudflare R2) |
| `S3_ACCESS_KEY_ID` | string | `""` | S3 Access Key ID |
| `S3_SECRET_ACCESS_KEY` | string | `""` | S3 Secret Access Key |
| `S3_ENDPOINT` | string | `""` | Custom S3 endpoint URL (R2, MinIO, Wasabi) |
| `S3_FORCE_PATH_STYLE` | bool | `false` | `true` for MinIO / LocalStack |
| `S3_USE_SSL` | bool | `true` | `false` only for local non-TLS MinIO |

---

## Health Check & Operations

```bash
curl -f http://127.0.0.1:8080/healthz
sudo systemctl status pdf-splitter
sudo journalctl -u pdf-splitter -f
```

## Building & Testing

```bash
go test -race ./...
go vet ./...
./build.sh
```
