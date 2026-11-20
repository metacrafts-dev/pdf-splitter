# Pdf Splitter (GoLang) — Modern Web UI, VPS Server & CLI

A compiled, standalone PDF pdf splitter built in pure Go with zero CGo or external dependencies. It works seamlessly as an **offline CLI tool**, a **local desktop app**, and an **online VPS web service** with automatic ZIP packaging, S3-compatible cloud storage, and background TTL file cleanup.

---

## 🌟 Key Features

### 1. Modern Bilingual Web UI (Arabic Default & English)
- **Arabic as Default with Native RTL**: Starts in Arabic (`العربية`) with full right-to-left directionality, mirrored icons, and Arabic typography.
- **Bilingual i18n Switching**: Seamless one-click switching between Arabic and English via the header segmented control.
- **Real-Time Upload Progress Indicator**: Live percentage (0% to 100%), animated progress bar, transferred MB / total MB tracking, and status display.
- **Light & Crisp Aesthetic**: High-contrast, modern light theme (`#f8fafc`, `#ffffff`, `#0f172a`).
- **Rectangular Lines & Selection Borders**: Crisp rectangular framing for dropzones, division cards, and split tags.
- **Modern Rounded Cards & Pill Buttons**: Soft rounded container cards (`border-radius: 16px`) and smooth action pills.
- **Redesigned Remainder / Rest-of-Book Cell**: Sharp rectangular amber callout (`border-left: 6px solid #d97706`, `background: #fffbeb`) displaying part number, exact page range, and remaining page count.
- **Spaced Division Cards**: Clean vertical spacing (`gap: 14px`) separating each calculated division.
- **Flexible Split Inputs**: Enter comma-separated or space-separated numbers at once (e.g. `10, 20, 40`), or add them individually.

### 2. ZIP Bundling & Automatic File Cleanup
- **Split & Bundle as ZIP**: Automatically generates a single compressed `.zip` archive containing all extracted parts.
- **One-Click Download**: Prominent **"Download All as ZIP"** button (with archive size indicator) and individual download buttons for each PDF part.
- **Automatic Server Retention (Auto-Purge)**: Background worker periodically purges session files older than the retention limit (default: `15m`, configurable with `RETENTION` or `-retention`).

### 3. S3 Cloud Storage & Local Fallback
- **Direct S3 Uploads**: Generates presigned PUT URLs for direct browser-to-S3 uploads, reducing server load.
- **Multi-Cloud Compatible**: Out-of-the-box support for AWS S3, Cloudflare R2, MinIO, Wasabi, and DigitalOcean Spaces.
- **Graceful Fallback**: If S3 credentials are not configured, seamlessly defaults to local filesystem sessions.

### 4. Tri-Mode Flexibility
- **Offline CLI Mode**: Fast, batch-scriptable command-line splitting with optional `-zip` bundling.
- **Local Desktop Mode**: Double-click execution with automatic desktop browser launch.
- **Online Production VPS Service**: Complete systemd hardening, environment file support, and Nginx reverse proxy configuration.

---

## 📐 Inclusive Splitting Logic

Split points mark the **inclusive end page** for each part:
- **Split at `10`** on a 100-page book:
  - **Part 1**: Pages 1 to 10 (10 pages)
  - **Part 2 (Rest)**: Pages 11 to 100 (90 pages)
- **Split at `10, 20, 40`** on a 100-page book:
  - **Part 1**: Pages 1 to 10 (10 pages)
  - **Part 2**: Pages 11 to 20 (10 pages)
  - **Part 3**: Pages 21 to 40 (20 pages)
  - **Part 4 (Rest)**: Pages 41 to 100 (60 pages)

---

## 🚀 How to Run

### Mode A: Local Desktop (Windows / Mac)
Double-click `pdf-plitter.exe` in Windows Explorer, or execute without arguments:
```powershell
.\pdf-plitter.exe
```
- **Automatic Browser Launch**: Launches the embedded server on `127.0.0.1:8080` and opens your default web browser automatically.
- **Standalone Auto-Shutdown**: In standalone mode, when the browser tab or window is closed, all session files are immediately deleted and the application turns off automatically.
- **15-Minute Data Retention**: Temporary sessions are kept for up to 15 minutes. Every session tracks its creation time (`meta.json`); if restarted, the server checks whether stale sessions have exceeded 15 minutes and purges them.

---

### Mode B: Command Line (CLI)

#### 1. Split and Create a ZIP Bundle:
```powershell
.\pdf-plitter.exe -input "path\to\book.pdf" -split "10,20,40" -zip
```

#### 2. Custom Output Directory:
```powershell
.\pdf-plitter.exe -input "book.pdf" -split "10,20,40" -output "D:\MySplitFolder" -zip
```

#### 3. Positional Syntax:
```powershell
.\pdf-plitter.exe "chemistry.pdf" 10 20 40 -zip
```

---

## 🌐 Production VPS Deployment Guide

Pdf Splitter includes enterprise-grade deployment automation located in the `deploy/` directory:
- [deploy/deploy.sh](file:///D:/WORKSPACE/PROJECTS/school-lessons/tools/pdf-plitter/deploy/deploy.sh): Automated, idempotent installer for Ubuntu, Debian, CentOS, Rocky Linux, and AlmaLinux.
- [deploy/pdf-plitter.service](file:///D:/WORKSPACE/PROJECTS/school-lessons/tools/pdf-plitter/deploy/pdf-plitter.service): Hardened systemd service with sandbox security and automatic restart.
- [deploy/nginx.conf](file:///D:/WORKSPACE/PROJECTS/school-lessons/tools/pdf-plitter/deploy/nginx.conf): High-performance Nginx reverse proxy supporting up to 500MB uploads, streaming, and extended timeouts.
- [.env.example](file:///D:/WORKSPACE/PROJECTS/school-lessons/tools/pdf-plitter/.env.example): Fully documented configuration template for server and S3 settings.

### Option 1: Automated 1-Minute Deployment (`deploy.sh`)

On your Linux server (Ubuntu/Debian/CentOS):

1. Upload or copy the `pdf-plitter` directory (or binary + `deploy/` folder) to your server.
2. Run the deployment script with root / sudo privileges:
   ```bash
   cd pdf-plitter
   sudo bash deploy/deploy.sh
   ```

**What the script does automatically:**
- Verifies system architecture (`x86_64` / `amd64`) and prerequisites.
- Creates an unprivileged system user and group (`pdfsplitter`).
- Sets up `/opt/pdf-plitter` and `/opt/pdf-plitter/data/sessions` with strict permissions (`750` / `770`).
- Installs the standalone Linux binary `pdf-plitter-linux-amd64` to `/opt/pdf-plitter/`.
- Initializes `/opt/pdf-plitter/.env` from `.env.example` with secure permissions (`chmod 600`).
- Configures and hardens `/etc/systemd/system/pdf-plitter.service`.
- Enables and starts the service, then runs an active HTTP health probe.

---

### Option 2: Manual Step-by-Step Deployment

If you prefer to configure the server manually:

#### 1. Create Dedicated Service Account
```bash
sudo useradd --system --shell /bin/false --home-dir /opt/pdf-plitter pdfsplitter
```

#### 2. Setup Directory Structure
```bash
sudo mkdir -p /opt/pdf-plitter/data/sessions
sudo cp pdf-plitter-linux-amd64 /opt/pdf-plitter/
sudo chmod 755 /opt/pdf-plitter/pdf-plitter-linux-amd64
```

#### 3. Configure Environment Variables
```bash
sudo cp .env.example /opt/pdf-plitter/.env
sudo chmod 600 /opt/pdf-plitter/.env
sudo nano /opt/pdf-plitter/.env
```
Ensure permissions belong to `pdfsplitter`:
```bash
sudo chown -R pdfsplitter:pdfsplitter /opt/pdf-plitter
sudo chmod 750 /opt/pdf-plitter
sudo chmod 770 /opt/pdf-plitter/data /opt/pdf-plitter/data/sessions
```

#### 4. Install & Start Systemd Service
```bash
sudo cp deploy/pdf-plitter.service /etc/systemd/system/
sudo chmod 644 /etc/systemd/system/pdf-plitter.service
sudo systemctl daemon-reload
sudo systemctl enable pdf-plitter
sudo systemctl start pdf-plitter
sudo systemctl status pdf-plitter
```

---

## 🔒 Systemd Security Hardening

The provided `deploy/pdf-plitter.service` utilizes advanced Linux kernel sandboxing:
- **`NoNewPrivileges=true`**: Prevents child processes from gaining elevated root privileges.
- **`ProtectSystem=strict`**: Mounts the entire OS filesystem as strictly read-only, whitelisting only `/opt/pdf-plitter/data` for uploads and temporary splits.
- **`ProtectHome=true`**: Blocks any access to user home directories (`/root`, `/home`).
- **`PrivateTmp=true`**: Provides an isolated `/tmp` namespace invisible to other host processes.
- **`PrivateDevices=true`**: Blocks physical device nodes.
- **`ProtectKernelTunables=true`** & **`ProtectKernelModules=true`**: Disallows kernel modification.
- **`MemoryDenyWriteExecute=true`**: Prevents creation of writable and executable memory pages (W^X violation defense).
- **`Restart=always`** & **`RestartSec=5s`**: Recovers immediately from unexpected failures.

---

## 🌐 Nginx Reverse Proxy & SSL Setup

For production deployments on a public domain with HTTPS:

### 1. Install Nginx Configuration
```bash
sudo cp deploy/nginx.conf /etc/nginx/sites-available/pdf-plitter.conf
sudo ln -s /etc/nginx/sites-available/pdf-plitter.conf /etc/nginx/sites-enabled/
```

### 2. Configure Domain Name
Edit `/etc/nginx/sites-available/pdf-plitter.conf` and update `server_name`:
```nginx
server_name splitter.yourdomain.com;
```

### 3. Verify & Reload Nginx
```bash
sudo nginx -t
sudo systemctl reload nginx
```

### 4. Enable Free SSL via Let's Encrypt Certbot
```bash
sudo certbot --nginx -d splitter.yourdomain.com
```

### Key Nginx Optimizations in `deploy/nginx.conf`:
- **`client_max_body_size 500M;`**: Allows uploading heavy multi-chapter textbook PDFs.
- **`proxy_buffering off;` & `proxy_request_buffering off;`**: Streamlines uploads and downloads directly without spooling large bodies to intermediate disk buffers.
- **`proxy_read_timeout 300s;`**: Accommodates processing times when splitting massive PDFs and generating ZIP archives.

---

## ⚙️ Environment Variables Reference

Pdf Splitter supports configuration through `.env` files (via systemd or Docker) and standard shell environment variables:

| Variable | Type | Default | Description |
|---|---|---|---|
| `PORT` | int | `8080` | TCP port for the web server to listen on |
| `HOST` | string | `0.0.0.0` | Host IP to bind server (`127.0.0.1` when behind Nginx) |
| `RETENTION` | string | `30m` | Time before session uploads and splits are auto-purged (e.g. `15m`, `1h`, `24h`) |
| `HEADLESS` | bool | `true` | Skip opening a local desktop browser (recommended on VPS) |
| `S3_BUCKET` | string | `""` | S3 bucket name. If empty, local filesystem storage is used |
| `S3_REGION` | string | `us-east-1` | S3 region (e.g. `us-east-1`, `eu-west-1`, or `auto` for Cloudflare R2) |
| `S3_ACCESS_KEY_ID` | string | `""` | S3 API Access Key ID |
| `S3_SECRET_ACCESS_KEY` | string | `""` | S3 API Secret Access Key |
| `S3_ENDPOINT` | string | `""` | Custom endpoint URL for MinIO, Cloudflare R2, or Wasabi (leave empty for AWS) |
| `S3_FORCE_PATH_STYLE` | bool | `false` | Set to `true` for MinIO / path-style S3 URLs (`endpoint/bucket/key`) |
| `S3_USE_SSL` | bool | `true` | Set to `false` only for local non-SSL MinIO instances (`http://`) |

---

## ☁️ Object Storage Setup (AWS S3, Cloudflare R2, MinIO)

To store book uploads and split outputs in cloud storage instead of local disk, populate the `S3_*` variables in `/opt/pdf-plitter/.env`:

### AWS S3 Example:
```env
S3_BUCKET=my-school-books
S3_REGION=us-east-1
S3_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE
S3_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
S3_ENDPOINT=
S3_FORCE_PATH_STYLE=false
S3_USE_SSL=true
```

### Cloudflare R2 Example:
```env
S3_BUCKET=my-r2-books
S3_REGION=auto
S3_ACCESS_KEY_ID=a1b2c3d4e5f6g7h8...
S3_SECRET_ACCESS_KEY=1234567890abcdef...
S3_ENDPOINT=https://<CF_ACCOUNT_ID>.r2.cloudflarestorage.com
S3_FORCE_PATH_STYLE=false
S3_USE_SSL=true
```

### Self-Hosted MinIO Example:
```env
S3_BUCKET=school-books
S3_REGION=us-east-1
S3_ACCESS_KEY_ID=minioadmin
S3_SECRET_ACCESS_KEY=minioadmin
S3_ENDPOINT=http://127.0.0.1:9000
S3_FORCE_PATH_STYLE=true
S3_USE_SSL=false
```

After modifying `/opt/pdf-plitter/.env`, restart the service:
```bash
sudo systemctl restart pdf-plitter
```

---

## 📊 Service Operations & Management

```bash
# Check service status and uptime
sudo systemctl status pdf-plitter

# View real-time application logs
sudo journalctl -u pdf-plitter -f

# View recent 50 log lines
sudo journalctl -u pdf-plitter -n 50 --no-pager

# Restart service after configuration or binary update
sudo systemctl restart pdf-plitter

# Stop service
sudo systemctl stop pdf-plitter
```

---

## ⚙️ CLI Flags Reference

Command-line flags override environment variable defaults:

| Flag | Type | Default | Description |
|---|---|---|---|
| `-input`, `-i` | string | `""` | Path to input PDF book (CLI mode) |
| `-split`, `-s` | string | `""` | Comma- or space-separated split points (e.g. `"10,20,40"`) |
| `-output`, `-o` | string | `<stem>_split_parts` | Destination folder for split files |
| `-zip`, `-z` | bool | `false` | Also compress split parts into a `.zip` file |
| `-server` | bool | `false` | Force start HTTP web server |
| `-host` | string | `0.0.0.0` | IP/Host to bind the web server (env: `HOST`) |
| `-port`, `-p` | int | `8080` | Port for the web server (env: `PORT`) |
| `-retention` | duration | `15m` | Time before session files are auto-purged (env: `RETENTION`) |
| `-headless` | bool | `false` | Run server without opening local desktop browser (env: `HEADLESS`) |
| `-help`, `-h` | bool | `false` | Display help and usage instructions |

---

## ☁️ Publishing under a Cloudflare Subdomain

To publish Pdf Splitter directly under a public Cloudflare subdomain (e.g. `books.yourdomain.com` or `split.myschool.org`) with zero firewall ports opened, automatic Cloudflare SSL/TLS, and DDoS mitigation:

### On your Linux VPS:
Run the automated publishing script:
```bash
sudo bash deploy/publish-cloudflare.sh books.yourdomain.com 8080
```

The script automatically:
1. Detects or installs the official `cloudflared` agent.
2. Verifies your Cloudflare login (`cert.pem`) or runs `cloudflared tunnel login`.
3. Creates a named persistent tunnel (`pdf-plitter`).
4. Creates a Cloudflare DNS CNAME record routing `books.yourdomain.com` to the tunnel.
5. Generates the ingress configuration in `/etc/cloudflared/config.yml` proxying to `http://127.0.0.1:8080`.
6. Installs and enables the `cloudflared` systemd service so it auto-starts on boot.

### Service Commands:
```bash
# Check tunnel status
sudo systemctl status cloudflared

# View real-time tunnel logs
sudo journalctl -u cloudflared -f

# Restart tunnel
sudo systemctl restart cloudflared
```

---

## 🔨 Building from Source & Distribution

All compiled binaries and runtime session storage reside cleanly in the `dist/` folder:

```
dist/
├── pdf-plitter.exe            # Windows compiled standalone binary
├── pdf-plitter-linux-amd64    # Linux 64-bit server binary
└── data/
    └── sessions/                # Ephemeral session directories & uploads
```

### Recompile into `dist/`:

**Windows (PowerShell):**
```powershell
.\build.ps1
```

**Linux / macOS (Bash):**
```bash
chmod +x build.sh
./build.sh
```

**Manual Compilation:**
```powershell
# Windows
go build -ldflags="-s -w" -o dist/pdf-plitter.exe .

# Linux amd64
$env:GOOS = "linux"; $env:GOARCH = "amd64"
go build -ldflags="-s -w" -o dist/pdf-plitter-linux-amd64 .
```
No CGo or GCC toolchains required.
