#!/usr/bin/env bash
# ==============================================================================
# PDF Splitter — Production Deployment & Service Setup Script
# Supported OS: Ubuntu 18.04+, Debian 10+, CentOS / RHEL 7+, Rocky Linux, AlmaLinux
# ==============================================================================
set -euo pipefail

APP_NAME="pdf-splitter"
LEGACY_APP_NAME="pdf-plitter"
INSTALL_DIR="/opt/${APP_NAME}"
DATA_DIR="${INSTALL_DIR}/data"
SESSIONS_DIR="${DATA_DIR}/sessions"
SERVICE_USER="pdfsplitter"
SERVICE_GROUP="pdfsplitter"
SERVICE_FILE="pdf-splitter.service"
SYSTEMD_PATH="/etc/systemd/system/${SERVICE_FILE}"
BINARY_NAME="pdf-splitter-linux-amd64"

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

log_title "Verifying System & Prerequisites"

if [[ $EUID -ne 0 ]]; then
    log_error "This script must be run as root or with sudo:"
    echo "  sudo bash $0"
    exit 1
fi

ARCH=$(uname -m)
if [[ "${ARCH}" != "x86_64" && "${ARCH}" != "amd64" ]]; then
    log_warn "Current architecture is '${ARCH}'. Standard binary is compiled for linux/amd64."
fi

if [[ -f /etc/os-release ]]; then
    . /etc/os-release
    log_info "Detected OS: ${NAME:-Linux} ${VERSION_ID:-}"
fi

if ! command -v systemctl >/dev/null 2>&1; then
    log_error "systemctl not found. A systemd-based Linux distribution is required."
    exit 1
fi

# Stop and disable legacy typo service unit if present
if systemctl list-unit-files | grep -q "^${LEGACY_APP_NAME}\.service"; then
    log_warn "Found legacy '${LEGACY_APP_NAME}.service'; stopping and disabling before migrating to '${APP_NAME}'."
    systemctl stop "${LEGACY_APP_NAME}" || true
    systemctl disable "${LEGACY_APP_NAME}" || true
fi

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
        --comment "PDF Splitter Service User" \
        "${SERVICE_USER}"
else
    log_info "Service user '${SERVICE_USER}' already exists."
fi

log_title "Setting up Directories in ${INSTALL_DIR}"

mkdir -p "${INSTALL_DIR}" "${DATA_DIR}" "${SESSIONS_DIR}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PARENT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

log_title "Installing Application Binary"

SRC_BINARY=""
for candidate in \
    "${SCRIPT_DIR}/${BINARY_NAME}" \
    "${PARENT_DIR}/dist/${BINARY_NAME}" \
    "${PARENT_DIR}/${BINARY_NAME}" \
    "./dist/${BINARY_NAME}" \
    "./${BINARY_NAME}"; do
    if [[ -f "${candidate}" ]]; then
        SRC_BINARY="${candidate}"
        break
    fi
done

if [[ -n "${SRC_BINARY}" ]]; then
    log_info "Installing ${SRC_BINARY} -> ${INSTALL_DIR}/${BINARY_NAME}"
    install -m 755 "${SRC_BINARY}" "${INSTALL_DIR}/${BINARY_NAME}.new"
    mv -f "${INSTALL_DIR}/${BINARY_NAME}.new" "${INSTALL_DIR}/${BINARY_NAME}"
elif [[ ! -f "${INSTALL_DIR}/${BINARY_NAME}" ]]; then
    log_error "Could not find '${BINARY_NAME}' in script, dist/, or parent directories!"
    exit 1
fi

log_title "Configuring Environment (.env)"

if [[ -f "${PARENT_DIR}/.env.example" ]]; then
    cp -f "${PARENT_DIR}/.env.example" "${INSTALL_DIR}/.env.example"
fi

if [[ ! -f "${INSTALL_DIR}/.env" ]]; then
    if [[ -f "${INSTALL_DIR}/.env.example" ]]; then
        log_info "Initializing ${INSTALL_DIR}/.env from .env.example"
        cp "${INSTALL_DIR}/.env.example" "${INSTALL_DIR}/.env"
    else
        log_info "Generating default ${INSTALL_DIR}/.env"
        cat <<EOF > "${INSTALL_DIR}/.env"
PORT=8080
HOST=127.0.0.1
RETENTION=15m
MAX_UPLOAD_MB=500
SESSIONS_DIR=${SESSIONS_DIR}
HEADLESS=true
EOF
    fi
    chmod 600 "${INSTALL_DIR}/.env"
    log_info "Created ${INSTALL_DIR}/.env with restrictive permissions (chmod 600)."
else
    log_info "${INSTALL_DIR}/.env already exists. Preserving existing configuration."
    if ! grep -qE '^SESSIONS_DIR=.+' "${INSTALL_DIR}/.env"; then
        log_warn "Adding missing SESSIONS_DIR=${SESSIONS_DIR} to existing ${INSTALL_DIR}/.env"
        echo "SESSIONS_DIR=${SESSIONS_DIR}" >> "${INSTALL_DIR}/.env"
    fi
fi

chown -R "${SERVICE_USER}:${SERVICE_GROUP}" "${INSTALL_DIR}"
chmod 750 "${INSTALL_DIR}"
chmod 770 "${DATA_DIR}" "${SESSIONS_DIR}"

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
    log_error "Service unit file ${SERVICE_FILE} not found."
    exit 1
fi

chmod 644 "${SYSTEMD_PATH}"

systemctl daemon-reload
systemctl enable "${APP_NAME}"
systemctl restart "${APP_NAME}"

log_title "Verifying Service Status"

sleep 2

if systemctl is-active --quiet "${APP_NAME}"; then
    log_info "Service '${APP_NAME}' is ACTIVE and RUNNING!"
else
    log_error "Service '${APP_NAME}' failed to start. Checking journal logs:"
    journalctl -u "${APP_NAME}" -n 25 --no-pager
    exit 1
fi

SVC_PORT="8080"
if [[ -f "${INSTALL_DIR}/.env" ]]; then
    ENV_PORT=$(grep -E '^PORT=' "${INSTALL_DIR}/.env" | cut -d'=' -f2 | tr -d ' "\r'\''' || true)
    if [[ -n "${ENV_PORT}" ]]; then
        SVC_PORT="${ENV_PORT}"
    fi
fi

if command -v curl >/dev/null 2>&1; then
    log_info "Probing http://127.0.0.1:${SVC_PORT}/healthz..."
    if curl -s -f -o /dev/null --connect-timeout 3 "http://127.0.0.1:${SVC_PORT}/healthz"; then
        log_info "Health probe succeeded on port ${SVC_PORT}."
    else
        log_warn "Local health probe failed. Check: sudo journalctl -u ${APP_NAME} -n 50"
    fi
fi

log_title "Deployment Complete!"
echo -e "  Binary:       ${INSTALL_DIR}/${BINARY_NAME}"
echo -e "  Environment:  ${INSTALL_DIR}/.env"
echo -e "  Data Dir:     ${DATA_DIR}"
echo -e "  Systemd Unit: ${SYSTEMD_PATH}"
echo -e "  Listen Port:  127.0.0.1:${SVC_PORT}"
