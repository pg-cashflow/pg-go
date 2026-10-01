#!/usr/bin/env bash
# install.sh: Copies systemd services and timers to /etc/systemd/system and enables timers.
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
   echo "Error: This script must be run as root (or with sudo)" 1>&2
   exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET_DIR="/etc/systemd/system"

echo "==> Installing pg-app background systemd services and timers..."

JOBS=(
    "pg-billing-cycle"
    "pg-cashfree-poll"
    "pg-digilocker-reconcile"
    "pg-financial-summary"
    "pg-gamification-cycle"
    "pg-kpi-snapshot"
    "pg-kyc-expiry"
    "pg-reminder"
    "pg-search-reindex"
)

for job in "${JOBS[@]}"; do
    echo "  -> Installing ${job}.service and ${job}.timer"
    cp "${SCRIPT_DIR}/${job}.service" "${TARGET_DIR}/"
    cp "${SCRIPT_DIR}/${job}.timer" "${TARGET_DIR}/"
    chmod 644 "${TARGET_DIR}/${job}.service" "${TARGET_DIR}/${job}.timer"
done

echo "==> Reloading systemd daemon..."
systemctl daemon-reload

echo "==> Enabling and starting all timers..."
for job in "${JOBS[@]}"; do
    echo "  -> Activating ${job}.timer"
    systemctl enable --now "${job}.timer"
done

echo "==> Timer status summary:"
systemctl list-timers --all | grep pg- || true

echo "==> All 9 background job timers installed and activated successfully!"
