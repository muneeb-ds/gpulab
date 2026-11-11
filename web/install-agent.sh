#!/usr/bin/env bash
# Installs gpulab-agent on a GPU worker and joins it to a control plane.
#   curl -fsSL https://CONTROL/install-agent.sh | sudo bash -s -- --server https://CONTROL --join-token TOKEN
set -euo pipefail

SERVER="" TOKEN="" INCUS_URL=""
while [ $# -gt 0 ]; do
  case "$1" in
    --server) SERVER="$2"; shift 2 ;;
    --join-token) TOKEN="$2"; shift 2 ;;
    --incus-url) INCUS_URL="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[ -n "$SERVER" ] && [ -n "$TOKEN" ] || { echo "usage: --server URL --join-token TOKEN [--incus-url https://IP:8443]" >&2; exit 2; }
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }

fail() { echo "error: $*" >&2; exit 1; }
command -v nvidia-smi >/dev/null || fail "NVIDIA driver not found (nvidia-smi missing)"
command -v incus >/dev/null || fail "Incus not found; install it and run 'incus admin init' first"
command -v nvidia-container-cli >/dev/null || fail "nvidia-container-toolkit not found (needed for nvidia.runtime=true)"
incus info >/dev/null 2>&1 || fail "Incus daemon not reachable; run 'incus admin init'"

if [ -z "$(incus config get core.https_address)" ]; then
  echo "Exposing the Incus API on :8443 (the control plane connects to it with a pinned client certificate)"
  incus config set core.https_address :8443
fi

echo "Downloading gpulab-agent from $SERVER"
curl -fsSL "$SERVER/download/gpulab-agent" -o /usr/local/bin/gpulab-agent.new
chmod 0755 /usr/local/bin/gpulab-agent.new
mv /usr/local/bin/gpulab-agent.new /usr/local/bin/gpulab-agent

install -d -m 0700 /var/lib/gpulab-agent
umask 077
cat > /etc/default/gpulab-agent <<ENV
GPULAB_SERVER=$SERVER
GPULAB_JOIN_TOKEN=$TOKEN
ENV
EXTRA=""
[ -n "$INCUS_URL" ] && EXTRA="--incus-url $INCUS_URL"

cat > /etc/systemd/system/gpulab-agent.service <<UNIT
[Unit]
Description=gpulab GPU worker agent
After=network-online.target incus.service
Wants=network-online.target

[Service]
EnvironmentFile=/etc/default/gpulab-agent
ExecStart=/usr/local/bin/gpulab-agent $EXTRA
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now gpulab-agent
echo "gpulab-agent is running. Approve this worker in the gpulab UI."
echo "Logs: journalctl -u gpulab-agent -f"
