#!/usr/bin/env bash
# ==============================================================================
# PDF Splitter — Cloudflare Subdomain Publishing & Tunnel Automation Script
# Connects local PDF Splitter service to a public Cloudflare subdomain.
#
# NOTE: Cloudflare Free/Pro plans enforce a 100 MB HTTP request body limit.
# For PDFs larger than 100 MB, configure S3/R2 storage in /opt/pdf-splitter/.env
# so uploads go directly from the browser to object storage.
# ==============================================================================
set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }
log_title() { echo -e "\n${BOLD}${CYAN}=== $* ===${NC}\n"; }

if [[ $EUID -ne 0 ]]; then
    log_error "This script must be run as root or with sudo:"
    echo "  sudo bash $0 <subdomain> [port] [--force]"
    exit 1
fi

FORCE_OVERWRITE=false
POSITIONAL=()
for arg in "$@"; do
    if [[ "${arg}" == "--force" ]]; then
        FORCE_OVERWRITE=true
    else
        POSITIONAL+=("${arg}")
    fi
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PARENT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

DEFAULT_PORT=8080
for env_file in "/opt/pdf-splitter/.env" "${PARENT_DIR}/.env"; do
    if [[ -f "${env_file}" ]]; then
        ENV_PORT=$(grep -E '^PORT=' "${env_file}" | cut -d'=' -f2 | tr -d ' "\r'\''' || true)
        if [[ -n "${ENV_PORT}" ]]; then
            DEFAULT_PORT="${ENV_PORT}"
            break
        fi
    fi
done

SUBDOMAIN="${POSITIONAL[0]:-}"
PORT="${POSITIONAL[1]:-${DEFAULT_PORT}}"

if [[ -z "${SUBDOMAIN}" ]]; then
    echo -e "${BOLD}Please enter the Cloudflare subdomain to publish PDF Splitter under:${NC}"
    read -rp "Subdomain: " SUBDOMAIN
fi

SUBDOMAIN=$(echo "${SUBDOMAIN}" | tr -d ' ' | tr '[:upper:]' '[:lower:]')
if [[ -z "${SUBDOMAIN}" || ! "${SUBDOMAIN}" =~ ^[a-z0-9.-]+$ ]]; then
    log_error "Invalid subdomain: '${SUBDOMAIN}'"
    exit 1
fi

log_warn "Cloudflare Free/Pro plans enforce a 100 MB HTTP upload limit."
log_warn "Configure S3/R2 in /opt/pdf-splitter/.env if users upload PDFs > 100 MB."

log_title "Checking Cloudflare Tunnel Agent (cloudflared)"

if ! command -v cloudflared >/dev/null 2>&1; then
    log_error "cloudflared is not installed."
    echo "Install it from Cloudflare's official package repository (with GPG verification) first:"
    echo "  https://pkg.cloudflare.com/"
    echo "Then re-run this script."
    exit 1
fi
CLOUDFLARED_BIN="$(command -v cloudflared)"
log_info "Found cloudflared at ${CLOUDFLARED_BIN}: $(${CLOUDFLARED_BIN} --version)"

log_title "Verifying Cloudflare Authentication"

CERT_PATH=""
POSSIBLE_CERTS=(
    "${HOME}/.cloudflared/cert.pem"
    "/root/.cloudflared/cert.pem"
    "/etc/cloudflared/cert.pem"
)
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
    log_warn "No Cloudflare origin certificate found (cert.pem). Running login..."
    "${CLOUDFLARED_BIN}" tunnel login
    for c in "${POSSIBLE_CERTS[@]}"; do
        if [[ -f "${c}" ]]; then
            CERT_PATH="${c}"
            break
        fi
    done
    if [[ -z "${CERT_PATH}" ]]; then
        log_error "Cloudflare authentication certificate not found."
        exit 1
    fi
fi

mkdir -p /etc/cloudflared
if [[ "${CERT_PATH}" != "/etc/cloudflared/cert.pem" ]]; then
    cp -f "${CERT_PATH}" /etc/cloudflared/cert.pem
    chmod 600 /etc/cloudflared/cert.pem
fi

TUNNEL_NAME="pdf-splitter"
log_title "Configuring Cloudflare Tunnel (${TUNNEL_NAME})"

TUNNEL_ID=$("${CLOUDFLARED_BIN}" tunnel list 2>/dev/null | awk -v name="${TUNNEL_NAME}" '$2 == name { print $1; exit }' || true)

if [[ -z "${TUNNEL_ID}" ]]; then
    log_info "Creating new Cloudflare Tunnel: ${TUNNEL_NAME}..."
    CREATE_OUT=$("${CLOUDFLARED_BIN}" tunnel create "${TUNNEL_NAME}" 2>&1)
    echo "${CREATE_OUT}"
    TUNNEL_ID=$(echo "${CREATE_OUT}" | grep -oE '[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}' | head -n1 || true)
else
    log_info "Reusing existing tunnel: ${TUNNEL_NAME} (${TUNNEL_ID})"
fi

if [[ -z "${TUNNEL_ID}" ]]; then
    log_error "Could not determine Tunnel UUID for '${TUNNEL_NAME}'."
    exit 1
fi

CRED_DEST="/etc/cloudflared/${TUNNEL_ID}.json"
for cred in "${HOME}/.cloudflared/${TUNNEL_ID}.json" "/root/.cloudflared/${TUNNEL_ID}.json" "${SUDO_HOME:-/nonexistent}/.cloudflared/${TUNNEL_ID}.json"; do
    if [[ -f "${cred}" && "${cred}" != "${CRED_DEST}" ]]; then
        cp -f "${cred}" "${CRED_DEST}"
        break
    fi
done
if [[ ! -f "${CRED_DEST}" ]]; then
    log_error "Tunnel credentials file not found: ${CRED_DEST}"
    exit 1
fi
chmod 600 "${CRED_DEST}"

log_title "Routing DNS Subdomain: ${SUBDOMAIN}"
"${CLOUDFLARED_BIN}" tunnel route dns -f "${TUNNEL_NAME}" "${SUBDOMAIN}"

# Use a dedicated config file and systemd unit so pre-existing cloudflared tunnels on this host are never clobbered.
CONF_FILE="/etc/cloudflared/${TUNNEL_NAME}.yml"
if [[ -f "${CONF_FILE}" && "${FORCE_OVERWRITE}" != "true" ]]; then
    BACKUP="${CONF_FILE}.bak.$(date +%Y%m%d%H%M%S)"
    cp -f "${CONF_FILE}" "${BACKUP}"
    log_info "Backed up existing ${CONF_FILE} -> ${BACKUP}"
fi

cat <<EOF > "${CONF_FILE}"
tunnel: ${TUNNEL_ID}
credentials-file: ${CRED_DEST}

ingress:
  - hostname: ${SUBDOMAIN}
    service: http://127.0.0.1:${PORT}
    originRequest:
      connectTimeout: 30s
      noTLSVerify: false
  - service: http_status:404
EOF
chmod 640 "${CONF_FILE}"

# Create dedicated unprivileged user for the tunnel service
if ! id -u cloudflared-pdf >/dev/null 2>&1; then
    useradd --system --shell /bin/false --no-create-home cloudflared-pdf
fi
chown root:cloudflared-pdf "${CONF_FILE}" "${CRED_DEST}"
chmod 640 "${CONF_FILE}" "${CRED_DEST}"

UNIT_PATH="/etc/systemd/system/cloudflared-pdf-splitter.service"
cat <<EOF > "${UNIT_PATH}"
[Unit]
Description=Cloudflare Tunnel for PDF Splitter
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
User=cloudflared-pdf
Group=cloudflared-pdf
TimeoutStartSec=0
ExecStart=${CLOUDFLARED_BIN} --config ${CONF_FILE} --no-autoupdate tunnel run
Restart=on-failure
RestartSec=5s
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable cloudflared-pdf-splitter
systemctl restart cloudflared-pdf-splitter

sleep 2
if systemctl is-active --quiet cloudflared-pdf-splitter; then
    log_info "cloudflared-pdf-splitter is RUNNING -> https://${SUBDOMAIN}"
else
    log_warn "Service status check returned non-active; inspect with: journalctl -u cloudflared-pdf-splitter -n 20"
fi
