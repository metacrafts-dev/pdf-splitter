#!/usr/bin/env bash
# ==============================================================================
# Pdf Splitter — Cloudflare Subdomain Publishing & Tunnel Automation Script
# Connects local Pdf Splitter service directly to a public Cloudflare subdomain
# with zero open firewall ports, automatic HTTPS/SSL, and DDoS protection.
# ==============================================================================
set -euo pipefail

# Color formatting
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $*"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $*"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $*" >&2
}

log_title() {
    echo -e "\n${BOLD}${CYAN}=== $* ===${NC}\n"
}

# ------------------------------------------------------------------------------
# 1. Root & OS Verification
# ------------------------------------------------------------------------------
log_title "Verifying System & Privileges"

if [[ $EUID -ne 0 ]]; then
    log_error "This script must be run as root or with sudo to configure systemd and /etc/cloudflared:"
    echo "  sudo bash $0 $*"
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PARENT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Detect local port from environment or .env file
DEFAULT_PORT=8080
if [[ -f "/opt/pdf-plitter/.env" ]]; then
    ENV_PORT=$(grep -E '^PORT=' /opt/pdf-plitter/.env | cut -d'=' -f2 | tr -d ' "\r' || true)
    if [[ -n "${ENV_PORT}" ]]; then
        DEFAULT_PORT="${ENV_PORT}"
    fi
elif [[ -f "${PARENT_DIR}/.env" ]]; then
    ENV_PORT=$(grep -E '^PORT=' "${PARENT_DIR}/.env" | cut -d'=' -f2 | tr -d ' "\r' || true)
    if [[ -n "${ENV_PORT}" ]]; then
        DEFAULT_PORT="${ENV_PORT}"
    fi
fi

SUBDOMAIN="${1:-}"
PORT="${2:-${DEFAULT_PORT}}"

# ------------------------------------------------------------------------------
# 2. Subdomain Input & Validation
# ------------------------------------------------------------------------------
if [[ -z "${SUBDOMAIN}" ]]; then
    echo -e "${BOLD}Please enter the Cloudflare subdomain to publish Pdf Splitter under:${NC}"
    echo "Examples: split.yourdomain.com, books.school.org, app.myvps.net"
    read -rp "Subdomain: " SUBDOMAIN
fi

SUBDOMAIN=$(echo "${SUBDOMAIN}" | tr -d ' ' | tr '[:upper:]' '[:lower:]')

if [[ -z "${SUBDOMAIN}" ]]; then
    log_error "Subdomain cannot be empty."
    exit 1
fi

log_info "Target Subdomain : ${BOLD}${SUBDOMAIN}${NC}"
log_info "Local Target Port: ${BOLD}${PORT}${NC} (http://127.0.0.1:${PORT})"

# ------------------------------------------------------------------------------
# 3. Check / Install cloudflared CLI
# ------------------------------------------------------------------------------
log_title "Checking Cloudflare Tunnel Agent (cloudflared)"

if ! command -v cloudflared >/dev/null 2>&1; then
    log_warn "cloudflared CLI not found in PATH. Installing latest official binary..."

    ARCH=$(uname -m)
    CLOUDFLARED_URL=""
    case "${ARCH}" in
        x86_64|amd64)
            CLOUDFLARED_URL="https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64"
            ;;
        aarch64|arm64)
            CLOUDFLARED_URL="https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-arm64"
            ;;
        armv7l|armhf)
            CLOUDFLARED_URL="https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-arm"
            ;;
        *)
            log_error "Unsupported architecture for automated cloudflared installation: ${ARCH}"
            echo "Please install cloudflared manually: https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/"
            exit 1
            ;;
    esac

    log_info "Downloading ${CLOUDFLARED_URL} -> /usr/local/bin/cloudflared"
    curl -fsSL --retry 3 "${CLOUDFLARED_URL}" -o /usr/local/bin/cloudflared
    chmod +x /usr/local/bin/cloudflared

    if ! command -v cloudflared >/dev/null 2>&1; then
        log_error "Failed to install cloudflared."
        exit 1
    fi
    log_info "cloudflared installed successfully: $(cloudflared --version)"
else
    log_info "cloudflared found: $(cloudflared --version)"
fi

# ------------------------------------------------------------------------------
# 4. Check Cloudflare Login / Origin Certificate
# ------------------------------------------------------------------------------
log_title "Verifying Cloudflare Authentication"

CERT_PATH=""
POSSIBLE_CERTS=(
    "${HOME}/.cloudflared/cert.pem"
    "/root/.cloudflared/cert.pem"
    "/etc/cloudflared/cert.pem"
)

# If original user invoked with sudo, check their home folder as well
if [[ -n "${SUDO_USER:-}" ]]; then
    SUDO_HOME=$(getent passwd "${SUDO_USER}" | cut -d: -f6)
    if [[ -n "${SUDO_HOME}" ]]; then
        POSSIBLE_CERTS+=("${SUDO_HOME}/.cloudflared/cert.pem")
    fi
fi

for c in "${POSSIBLE_CERTS[@]}"; do
    if [[ -f "${c}" ]]; then
        CERT_PATH="${c}"
        break
    fi
done

if [[ -z "${CERT_PATH}" ]]; then
    log_warn "No Cloudflare origin certificate found (cert.pem)."
    echo -e "${YELLOW}Please authenticate your Cloudflare account now.${NC}"
    echo "Running 'cloudflared tunnel login'..."
    cloudflared tunnel login

    # Check again
    for c in "${POSSIBLE_CERTS[@]}"; do
        if [[ -f "${c}" ]]; then
            CERT_PATH="${c}"
            break
        fi
    done

    if [[ -z "${CERT_PATH}" ]]; then
        log_error "Cloudflare authentication certificate was not detected. Cannot proceed."
        exit 1
    fi
fi

log_info "Using Cloudflare Certificate: ${CERT_PATH}"

# Ensure /etc/cloudflared directory exists
mkdir -p /etc/cloudflared
if [[ "${CERT_PATH}" != "/etc/cloudflared/cert.pem" ]]; then
    cp -f "${CERT_PATH}" /etc/cloudflared/cert.pem
    chmod 600 /etc/cloudflared/cert.pem
fi

# ------------------------------------------------------------------------------
# 5. Create or Reuse Tunnel
# ------------------------------------------------------------------------------
TUNNEL_NAME="pdf-plitter"
log_title "Configuring Cloudflare Tunnel (${TUNNEL_NAME})"

# Extract existing tunnel ID if present
TUNNEL_ID=$(cloudflared tunnel list --output json 2>/dev/null | grep -o "\"id\":\"[^\"]*\"" | grep -B1 "\"name\":\"${TUNNEL_NAME}\"" | head -n1 | cut -d'"' -f4 || true)

if [[ -z "${TUNNEL_ID}" ]]; then
    # Alternate parsing in case JSON output isn't supported
    TUNNEL_ID=$(cloudflared tunnel list 2>/dev/null | awk -v name="${TUNNEL_NAME}" '$2 == name { print $1 }' || true)
fi

if [[ -z "${TUNNEL_ID}" ]]; then
    log_info "Creating new Cloudflare Tunnel: ${TUNNEL_NAME}..."
    CREATE_OUT=$(cloudflared tunnel create "${TUNNEL_NAME}" 2>&1)
    echo "${CREATE_OUT}"

    # Extract UUID from output
    TUNNEL_ID=$(echo "${CREATE_OUT}" | grep -oE '[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}' | head -n1 || true)
    if [[ -z "${TUNNEL_ID}" ]]; then
        TUNNEL_ID=$(cloudflared tunnel list 2>/dev/null | awk -v name="${TUNNEL_NAME}" '$2 == name { print $1 }' || true)
    fi
else
    log_info "Found existing Cloudflare Tunnel: ${TUNNEL_NAME} (UUID: ${TUNNEL_ID})"
fi

if [[ -z "${TUNNEL_ID}" ]]; then
    log_error "Could not determine Tunnel UUID for '${TUNNEL_NAME}'."
    exit 1
fi

log_info "Tunnel ID: ${BOLD}${TUNNEL_ID}${NC}"

# Ensure credentials json exists in /etc/cloudflared
CRED_SOURCE=""
POSSIBLE_CREDS=(
    "${HOME}/.cloudflared/${TUNNEL_ID}.json"
    "/root/.cloudflared/${TUNNEL_ID}.json"
    "/etc/cloudflared/${TUNNEL_ID}.json"
)
if [[ -n "${SUDO_USER:-}" && -n "${SUDO_HOME:-}" ]]; then
    POSSIBLE_CREDS+=("${SUDO_HOME}/.cloudflared/${TUNNEL_ID}.json")
fi

for cred in "${POSSIBLE_CREDS[@]}"; do
    if [[ -f "${cred}" ]]; then
        CRED_SOURCE="${cred}"
        break
    fi
done

if [[ -n "${CRED_SOURCE}" && "${CRED_SOURCE}" != "/etc/cloudflared/${TUNNEL_ID}.json" ]]; then
    cp -f "${CRED_SOURCE}" "/etc/cloudflared/${TUNNEL_ID}.json"
fi

if [[ ! -f "/etc/cloudflared/${TUNNEL_ID}.json" ]]; then
    log_error "Tunnel credentials file not found: /etc/cloudflared/${TUNNEL_ID}.json"
    exit 1
fi
chmod 600 "/etc/cloudflared/${TUNNEL_ID}.json"

# ------------------------------------------------------------------------------
# 6. Route DNS Subdomain to Tunnel
# ------------------------------------------------------------------------------
log_title "Routing DNS Subdomain: ${SUBDOMAIN}"

log_info "Executing: cloudflared tunnel route dns ${TUNNEL_NAME} ${SUBDOMAIN}"
ROUTE_OUT=$(cloudflared tunnel route dns -f "${TUNNEL_NAME}" "${SUBDOMAIN}" 2>&1 || true)
echo "${ROUTE_OUT}"

if echo "${ROUTE_OUT}" | grep -iq "already exists"; then
    log_info "DNS record for ${SUBDOMAIN} already routes to this tunnel."
elif echo "${ROUTE_OUT}" | grep -iq "added"; then
    log_info "DNS CNAME successfully created for ${SUBDOMAIN} in Cloudflare!"
fi

# ------------------------------------------------------------------------------
# 7. Generate Cloudflare Tunnel Configuration
# ------------------------------------------------------------------------------
log_title "Generating Ingress Configuration (/etc/cloudflared/config.yml)"

cat <<EOF > /etc/cloudflared/config.yml
tunnel: ${TUNNEL_ID}
credentials-file: /etc/cloudflared/${TUNNEL_ID}.json

ingress:
  - hostname: ${SUBDOMAIN}
    service: http://127.0.0.1:${PORT}
    originRequest:
      connectTimeout: 30s
      noTLSVerify: false
  - service: http_status:404
EOF

chmod 644 /etc/cloudflared/config.yml
log_info "Configuration written to /etc/cloudflared/config.yml"

# ------------------------------------------------------------------------------
# 8. Setup & Start Systemd Service
# ------------------------------------------------------------------------------
log_title "Configuring & Starting cloudflared Service"

if [[ ! -f /etc/systemd/system/cloudflared.service ]]; then
    log_info "Installing cloudflared system service..."
    cloudflared service install 2>/dev/null || true
fi

# Ensure service file has correct start arguments
cat <<EOF > /etc/systemd/system/cloudflared.service
[Unit]
Description=Cloudflare Tunnel for Pdf Splitter
After=network-online.target
Wants=network-online.target

[Service]
TimeoutStartSec=0
Type=notify
ExecStart=/usr/local/bin/cloudflared --config /etc/cloudflared/config.yml --no-autoupdate tunnel run
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable cloudflared
systemctl restart cloudflared

sleep 2

if systemctl is-active --quiet cloudflared; then
    log_info "cloudflared service is RUNNING and enabled on boot!"
else
    log_warn "cloudflared service status check returned non-active. Checking logs:"
    journalctl -u cloudflared -n 15 --no-pager
fi

# ------------------------------------------------------------------------------
# 9. Verify Pdf Splitter Backend Service
# ------------------------------------------------------------------------------
log_title "Checking Pdf Splitter Backend"

if curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${PORT}/api/config" | grep -q "200"; then
    log_info "Pdf Splitter backend is responding on http://127.0.0.1:${PORT} (HTTP 200 OK)."
else
    log_warn "Pdf Splitter does not appear to be running on http://127.0.0.1:${PORT} yet."
    echo -e "To start the Pdf Splitter backend service, run:"
    echo -e "  ${BOLD}sudo systemctl start pdf-plitter${NC}"
fi

# ------------------------------------------------------------------------------
# 10. Publication Summary
# ------------------------------------------------------------------------------
log_title "🎉 Cloudflare Subdomain Successfully Published!"

echo -e "  🌐 ${BOLD}Public HTTPS URL:${NC}    ${CYAN}https://${SUBDOMAIN}${NC}"
echo -e "  🔗 ${BOLD}Tunnel Target:${NC}       http://127.0.0.1:${PORT}"
echo -e "  🛡️  ${BOLD}SSL / HTTPS:${NC}         Automatic Cloudflare Managed Edge SSL (Zero-Config)"
echo -e "  🚀 ${BOLD}DDoS & WAF:${NC}          Cloudflare Edge Network Protection"
echo -e "  ⚙️  ${BOLD}Tunnel Service:${NC}      cloudflared (systemd auto-restart)"
echo ""
echo -e "${BOLD}Management Commands:${NC}"
echo -e "  Check Tunnel Status:   ${CYAN}sudo systemctl status cloudflared${NC}"
echo -e "  View Live Tunnel Logs: ${CYAN}sudo journalctl -u cloudflared -f${NC}"
echo -e "  Restart Tunnel:        ${CYAN}sudo systemctl restart cloudflared${NC}"
echo -e "  Check Backend Status:  ${CYAN}sudo systemctl status pdf-plitter${NC}"
echo ""
echo -e "${GREEN}You can now open https://${SUBDOMAIN} in any web browser!${NC}"
