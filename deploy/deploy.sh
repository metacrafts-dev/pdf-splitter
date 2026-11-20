#!/usr/bin/env bash
# ==============================================================================
# Pdf Splitter — Production Deployment & Service Setup Script
# Supported OS: Ubuntu 18.04+, Debian 10+, CentOS / RHEL 7+, Rocky Linux, AlmaLinux
# ==============================================================================
set -euo pipefail

# Configuration
APP_NAME="pdf-plitter"
INSTALL_DIR="/opt/${APP_NAME}"
DATA_DIR="${INSTALL_DIR}/data"
SESSIONS_DIR="${DATA_DIR}/sessions"
SERVICE_USER="pdfsplitter"
SERVICE_GROUP="pdfsplitter"
SERVICE_FILE="pdf-plitter.service"
SYSTEMD_PATH="/etc/systemd/system/${SERVICE_FILE}"
BINARY_NAME="pdf-plitter-linux-amd64"

# Color formatting
RED='\033[0;32m'
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
log_title "Verifying System & Prerequisites"

if [[ $EUID -ne 0 ]]; then
    log_error "This script must be run as root or with sudo:"
    echo "  sudo bash $0"
    exit 1
fi

ARCH=$(uname -m)
if [[ "${ARCH}" != "x86_64" && "${ARCH}" != "amd64" ]]; then
    log_warn "Current architecture is '${ARCH}'. Standard binary is compiled for linux/amd64."
    log_warn "If this binary fails, cross-compile for your architecture with: GOARCH=${ARCH} go build"
fi

if [[ -f /etc/os-release ]]; then
    . /etc/os-release
    log_info "Detected OS: ${NAME:-Linux} ${VERSION_ID:-}"
else
    log_warn "Unable to detect OS via /etc/os-release. Proceeding with generic Linux setup..."
fi

# Check for systemd
if ! command -v systemctl >/dev/null 2>&1; then
    log_error "systemctl not found. A systemd-based Linux distribution is required."
    exit 1
fi

# ------------------------------------------------------------------------------
# 2. Dedicated Service User & Group
# ------------------------------------------------------------------------------
log_title "Configuring Service Account (${SERVICE_USER})"

if ! getent group "${SERVICE_GROUP}" >/dev/null 2>&1; then
    log_info "Creating group: ${SERVICE_GROUP}"
    groupadd --system "${SERVICE_GROUP}"
fi

if ! id -u "${SERVICE_USER}" >/dev/null 2>&1; then
    log_info "Creating system user: ${SERVICE_USER}"
    useradd --system \
        --gid "${SERVICE_GROUP}" \
        --home-dir "${INSTALL_DIR}" \
        --shell /bin/false \
        --comment "Pdf Splitter Service User" \
        "${SERVICE_USER}"
else
    log_info "Service user '${SERVICE_USER}' already exists."
fi

# ------------------------------------------------------------------------------
# 3. Directory Layout & Permissions
# ------------------------------------------------------------------------------
log_title "Setting up Directories in ${INSTALL_DIR}"

mkdir -p "${INSTALL_DIR}"
mkdir -p "${DATA_DIR}"
mkdir -p "${SESSIONS_DIR}"

# Determine directory where deploy.sh is located
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PARENT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ------------------------------------------------------------------------------
# 4. Installing Binary
# ------------------------------------------------------------------------------
log_title "Installing Application Binary"

SRC_BINARY=""
if [[ -f "${SCRIPT_DIR}/${BINARY_NAME}" ]]; then
    SRC_BINARY="${SCRIPT_DIR}/${BINARY_NAME}"
elif [[ -f "${PARENT_DIR}/dist/${BINARY_NAME}" ]]; then
    SRC_BINARY="${PARENT_DIR}/dist/${BINARY_NAME}"
elif [[ -f "${PARENT_DIR}/${BINARY_NAME}" ]]; then
    SRC_BINARY="${PARENT_DIR}/${BINARY_NAME}"
elif [[ -f "./dist/${BINARY_NAME}" ]]; then
    SRC_BINARY="./dist/${BINARY_NAME}"
elif [[ -f "./${BINARY_NAME}" ]]; then
    SRC_BINARY="./${BINARY_NAME}"
elif [[ -f "${INSTALL_DIR}/${BINARY_NAME}" ]]; then
    log_info "Binary already exists in ${INSTALL_DIR}/${BINARY_NAME}"
fi

if [[ -n "${SRC_BINARY}" ]]; then
    log_info "Copying ${SRC_BINARY} -> ${INSTALL_DIR}/${BINARY_NAME}"
    cp -f "${SRC_BINARY}" "${INSTALL_DIR}/${BINARY_NAME}"
elif [[ ! -f "${INSTALL_DIR}/${BINARY_NAME}" ]]; then
    log_error "Could not find '${BINARY_NAME}' in current, script, or parent directories!"
    echo "Please place '${BINARY_NAME}' into ${INSTALL_DIR} or run this script from the repository folder."
    exit 1
fi

chmod 755 "${INSTALL_DIR}/${BINARY_NAME}"

# ------------------------------------------------------------------------------
# 5. Environment File Setup
# ------------------------------------------------------------------------------
log_title "Configuring Environment (.env)"

# Install .env.example
if [[ -f "${PARENT_DIR}/.env.example" ]]; then
    cp -f "${PARENT_DIR}/.env.example" "${INSTALL_DIR}/.env.example"
elif [[ -f "${SCRIPT_DIR}/../.env.example" ]]; then
    cp -f "${SCRIPT_DIR}/../.env.example" "${INSTALL_DIR}/.env.example"
fi

if [[ ! -f "${INSTALL_DIR}/.env" ]]; then
    if [[ -f "${INSTALL_DIR}/.env.example" ]]; then
        log_info "Initializing ${INSTALL_DIR}/.env from .env.example"
        cp "${INSTALL_DIR}/.env.example" "${INSTALL_DIR}/.env"
    else
        log_info "Generating default ${INSTALL_DIR}/.env"
        cat <<EOF > "${INSTALL_DIR}/.env"
PORT=8080
HOST=0.0.0.0
RETENTION=15m
HEADLESS=true
EOF
    fi
    chmod 600 "${INSTALL_DIR}/.env"
    log_info "Created ${INSTALL_DIR}/.env with restrictive permissions (chmod 600)."
else
    log_info "${INSTALL_DIR}/.env already exists. Preserving existing configuration."
fi

# Set ownership
chown -R "${SERVICE_USER}:${SERVICE_GROUP}" "${INSTALL_DIR}"
chmod 750 "${INSTALL_DIR}"
chmod 770 "${DATA_DIR}"
chmod 770 "${SESSIONS_DIR}"

# ------------------------------------------------------------------------------
# 6. Systemd Service Installation
# ------------------------------------------------------------------------------
log_title "Configuring systemd Service"

SRC_SERVICE=""
if [[ -f "${SCRIPT_DIR}/${SERVICE_FILE}" ]]; then
    SRC_SERVICE="${SCRIPT_DIR}/${SERVICE_FILE}"
elif [[ -f "${PARENT_DIR}/deploy/${SERVICE_FILE}" ]]; then
    SRC_SERVICE="${PARENT_DIR}/deploy/${SERVICE_FILE}"
fi

if [[ -n "${SRC_SERVICE}" ]]; then
    log_info "Installing service unit: ${SRC_SERVICE} -> ${SYSTEMD_PATH}"
    cp -f "${SRC_SERVICE}" "${SYSTEMD_PATH}"
else
    log_info "Generating systemd service unit directly to ${SYSTEMD_PATH}"
    cat <<EOF > "${SYSTEMD_PATH}"
[Unit]
Description=Pdf Splitter Web Service - PDF Chapter & Range Splitter
Documentation=file:///opt/pdf-plitter/README.md
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
WorkingDirectory=${INSTALL_DIR}
EnvironmentFile=-${INSTALL_DIR}/.env
ExecStart=${INSTALL_DIR}/${BINARY_NAME} -server
Restart=always
RestartSec=5s
StartLimitIntervalSec=60s
StartLimitBurst=5
StandardOutput=journal
StandardError=journal
SyslogIdentifier=pdf-plitter
LimitNOFILE=65535
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=${DATA_DIR}
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
MemoryDenyWriteExecute=true
LockPersonality=true

[Install]
WantedBy=multi-user.target
EOF
fi

chmod 644 "${SYSTEMD_PATH}"

log_info "Reloading systemd daemon..."
systemctl daemon-reload

log_info "Enabling service on boot..."
systemctl enable "${APP_NAME}"

log_info "Starting service..."
systemctl restart "${APP_NAME}"

# ------------------------------------------------------------------------------
# 7. Verification & Status Health Check
# ------------------------------------------------------------------------------
log_title "Verifying Service Status"

sleep 2

if systemctl is-active --quiet "${APP_NAME}"; then
    log_info "Service '${APP_NAME}' is ACTIVE and RUNNING! \xE2\x9C\x94"
else
    log_error "Service '${APP_NAME}' failed to start. Checking journal logs:"
    journalctl -u "${APP_NAME}" -n 25 --no-pager
    exit 1
fi

# Extract port from .env or default to 8080
SVC_PORT="8080"
if [[ -f "${INSTALL_DIR}/.env" ]]; then
    ENV_PORT=$(grep -E "^PORT=" "${INSTALL_DIR}/.env" | cut -d'=' -f2 | tr -d ' "''' || true)
    if [[ -n "${ENV_PORT}" ]]; then
        SVC_PORT="${ENV_PORT}"
    fi
fi

if command -v curl >/dev/null 2>&1; then
    log_info "Performing local HTTP health probe on http://127.0.0.1:${SVC_PORT}/..."
    if curl -s -f -o /dev/null --connect-timeout 3 "http://127.0.0.1:${SVC_PORT}/"; then
        log_info "HTTP probe succeeded! Pdf Splitter is responding on port ${SVC_PORT}."
    else
        log_warn "Local HTTP request returned non-200 or timed out. Check service logs."
    fi
fi

# ------------------------------------------------------------------------------
# 8. Summary & Next Steps
# ------------------------------------------------------------------------------
log_title "Deployment Complete!"

echo -e "${BOLD}Service Details:${NC}"
echo -e "  Binary:       ${INSTALL_DIR}/${BINARY_NAME}"
echo -e "  Environment:  ${INSTALL_DIR}/.env"
echo -e "  Data Dir:     ${DATA_DIR}"
echo -e "  Systemd Unit: ${SYSTEMD_PATH}"
echo -e "  Listen Port:  ${SVC_PORT}"
echo ""
echo -e "${BOLD}Management Commands:${NC}"
echo -e "  Check status:   sudo systemctl status ${APP_NAME}"
echo -e "  Restart:        sudo systemctl restart ${APP_NAME}"
echo -e "  Stop:           sudo systemctl stop ${APP_NAME}"
echo -e "  View live logs: sudo journalctl -u ${APP_NAME} -f"
echo ""
echo -e "${BOLD}Next Steps:${NC}"
echo -e "  1. If using AWS S3 / Cloudflare R2 / MinIO, edit your credentials in:"
echo -e "     sudo nano ${INSTALL_DIR}/.env"
echo -e "     sudo systemctl restart ${APP_NAME}"
echo -e "  2. To configure Nginx as a reverse proxy with 500M uploads and SSL:"
echo -e "     Copy deploy/nginx.conf to /etc/nginx/sites-available/pdf-plitter.conf"
echo ""
