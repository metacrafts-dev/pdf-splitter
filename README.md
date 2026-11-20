# PDF Splitter (Go) — Web UI, VPS Server & CLI

A compiled, standalone PDF splitter built in pure Go (`CGO_ENABLED=0`) with zero external runtime dependencies. It runs as an **offline CLI tool**, a **local desktop app**, or a **hardened VPS web service** behind a Cloudflare Tunnel (or optional Nginx) with Store-mode ZIP packaging, S3-compatible object storage, and automatic local + S3 TTL cleanup.

---

## Key Features

### 1. Bilingual Web UI (Arabic Default with RTL & English)
- Starts in Arabic (`العربية`) with full RTL layout and one-click switching to English.
- Real-time upload progress bar with **live percentage, transferred MB, smoothed upload speed (B/s, KB/s, MB/s), estimated time remaining (ETA), client-side file-size pre-check, and mid-upload Cancel button**.
- Inclusive split-point preview showing exact page ranges and the remainder block.
- Strict Content Security Policy (`script-src 'self'`), per-boot CSRF tokens, and zero inline scripts.

### 2. Fast Single-Pass Splitting, Stateless `pdfcpu` & Store-Mode ZIP
- **Stateless `pdfcpu` execution (`model.NewStatelessConfiguration` + `api.DisableConfigDir`)**: never writes to `$HOME/.config` or `XDG_CONFIG_HOME`, working reliably under read-only sandboxes (`ProtectSystem=strict`, `ProtectHome=true`) without environment workarounds.
- **Single-pass `pdfcpu.SplitByPageNr`**: parses the source PDF once rather than once per output part.
- **Uncompressed (`zip.Store`) atomic ZIP archives**: avoids redundant Deflate CPU burn on already-compressed PDFs and writes via temp-file + atomic rename.
- **Automatic Retention (default `15m`)**: background cleaner purges expired session directories, evicts in-memory session metadata, and runs periodic paginated S3 cleanup.

### 3. S3 Cloud Storage & Local Fallback
- Direct browser-to-S3 uploads via SigV4 presigned PUT URLs binding exact `Content-Length` and `Content-Type: application/pdf`.
- Supports AWS S3, Cloudflare R2, MinIO, Wasabi, and DigitalOcean Spaces.
- Server-minted session keys prevent unauthorized S3 key access.

### 4. Security & Reliability Hardening
- Panic recovery middleware logs stack traces and returns structured JSON `500` responses instead of dropping connections (preventing opaque reverse-proxy `502` errors).
- Strict 32-char lowercase hex session ID validation and path containment across all endpoints.
- Desktop-only routes (`/api/events`, `/api/open-folder`, `/api/shutdown`, `/api/heartbeat`) are disabled in VPS (`-server`) mode.
- Hard `http.MaxBytesReader` upload limits (`MAX_UPLOAD_MB`), `32 MiB` multipart RAM cap, `%PDF-` header verification, concurrency semaphore, per-IP rate limiting (with `CF-Connecting-IP` support behind loopback proxies), and `MaxSplitPoints` cap (`200`).

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

### Mode C: Production VPS Deployment (Cloudflare Tunnel or Optional Nginx)
Deployment assets live in `deploy/`:
- [`deploy/deploy.sh`](deploy/deploy.sh): Idempotent Linux installer (Ubuntu/Debian/RHEL/Rocky/Alma).
- [`deploy/pdf-splitter.service`](deploy/pdf-splitter.service): Sandboxed systemd unit (`ProtectSystem=strict`, `NoNewPrivileges=true`, `MemoryDenyWriteExecute=true`, `[Unit]` restart rate limiting).
- [`deploy/publish-cloudflare.sh`](deploy/publish-cloudflare.sh): Dedicated Cloudflare Tunnel setup (recommended when exposing `http://127.0.0.1:8080` directly without Nginx).
- [`deploy/nginx.conf`](deploy/nginx.conf): Optional Nginx reverse proxy template (only needed if serving directly via Nginx instead of Cloudflare Tunnel).
- [`.env.example`](.env.example): Configuration template.

```bash
./build.sh
sudo bash deploy/deploy.sh
```

> **Cloudflare Tunnel Upload Limit (Important):** Cloudflare Free/Pro plans enforce a **100 MB** HTTP request body limit at the edge. When serving behind a Cloudflare Tunnel **without** S3/R2 configured, keep `MAX_UPLOAD_MB=100` in `/opt/pdf-splitter/.env` so the browser rejects oversized PDFs client-side with a friendly message instead of failing mid-upload at Cloudflare's edge with HTTP 413. To support PDFs up to `500 MB`, configure S3/R2 in `.env` so browser uploads bypass Cloudflare via presigned URLs.

> **Upgrading from an older `pdf-plitter` deployment:** `deploy/deploy.sh` automatically stops and disables any legacy `pdf-plitter.service` unit before installing `/opt/pdf-splitter` and `pdf-splitter.service`. Copy your existing `/opt/pdf-plitter/.env` to `/opt/pdf-splitter/.env` (updating `SESSIONS_DIR=/opt/pdf-splitter/data/sessions`) if needed.

---

## Environment Variables Reference

| Variable | Type | Default | Description |
|---|---|---|---|
| `PORT` | int | `8080` | TCP port to listen on |
| `HOST` | string | `127.0.0.1` | Host IP to bind (`127.0.0.1` behind Cloudflare Tunnel / Nginx) |
| `RETENTION` | duration | `15m` | Auto-purge TTL for local and S3 sessions |
| `MAX_UPLOAD_MB` | int | `500` (`100` in `.env.example`) | Max PDF upload size in MB (use `100` for Cloudflare Tunnel without S3) |
| `SESSIONS_DIR` | string | `<bin-dir>/data/sessions` | Session directory (`/opt/pdf-splitter/data/sessions` on VPS) |
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

## 🔏 Code Signing (removing "Unknown publisher")

Windows builds are signed with an Authenticode certificate from
[Certum's Open Source Code Signing](https://shop.certum.eu/open-source-code-signing-on-simplysign.html)
program (€49/yr, cloud-based, for non-commercial open-source projects).

Setup (one-time, on the build machine):
1. Purchase the certificate and complete identity verification (ID document + utility bill + this repo's URL).
2. Install [SimplySign Desktop](https://www.certum.eu/en/lp/simplysign/) and activate the certificate —
   it appears as a virtual smart card.

Sign and release:
```powershell
.\sign.ps1                    # signs dist\*.exe + timestamps
cd dist; sha256sum pdf-splitter.exe pdf-splitter-linux-amd64 > SHA256SUMS.txt
gh release create vX.Y.Z dist/* --generate-notes
```
