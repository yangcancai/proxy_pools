#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" -ne 0 ]]; then
  echo "Run as root, for example: sudo bash install.sh [subscription-url]" >&2
  exit 1
fi
if [[ $# -gt 1 ]]; then
  echo "Usage: install.sh [clash-subscription-url]" >&2
  exit 2
fi

SUBSCRIPTION_URL="${PROXY_SUBSCRIPTION_URL:-}"
if [[ $# -eq 1 ]]; then
  case "$1" in
    http://*|https://*) SUBSCRIPTION_URL="$1" ;;
    *) echo "Subscription URL must start with http:// or https://" >&2; exit 2 ;;
  esac
fi
if [[ $# -eq 0 && -z "$SUBSCRIPTION_URL" && -r /etc/proxy-pools/proxy-pools.env ]]; then
  SUBSCRIPTION_URL="$(sed -n 's/^PROXY_SUBSCRIPTION_URL=//p' /etc/proxy-pools/proxy-pools.env | head -n 1)"
fi
REPO="yangcancai/proxy_pools"

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
ASSET="proxy_pools_linux_${ARCH}"
DOWNLOAD_URL="https://github.com/${REPO}/releases/latest/download/${ASSET}"
RELEASE_VERSION="$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest" | sed 's#.*/tag/##')"

SCRIPT_DIR=""
if [[ -n "${BASH_SOURCE[0]:-}" && -f "${BASH_SOURCE[0]}" ]]; then
  SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
fi
ASSET_DIR=""
if [[ -z "$SCRIPT_DIR" || ! -f "$SCRIPT_DIR/pp" || ! -f "$SCRIPT_DIR/proxy-pools.service" ]]; then
  RAW_BASE="${PROXY_POOLS_RAW_BASE:-https://raw.githubusercontent.com/${REPO}/main/deploy}"
  ASSET_DIR="$(mktemp -d)"
  SCRIPT_DIR="$ASSET_DIR"
  curl -fsSL "$RAW_BASE/pp" -o "$SCRIPT_DIR/pp"
  curl -fsSL "$RAW_BASE/proxy-pools.service" -o "$SCRIPT_DIR/proxy-pools.service"
fi

install -d -m 0755 /opt/proxy-pools /etc/proxy-pools /var/lib/proxy-pools /usr/local/libexec
if ! id proxy-pools >/dev/null 2>&1; then
  useradd --system --home-dir /home/proxy-pools --create-home --shell /usr/sbin/nologin proxy-pools
fi
tmp="$(mktemp)"
trap 'rm -f "${tmp:-}"; if [[ -n "${ASSET_DIR:-}" ]]; then rm -rf "$ASSET_DIR"; fi' EXIT
curl -fL --retry 3 -o "$tmp" "$DOWNLOAD_URL"
install -o root -g root -m 0755 "$tmp" /opt/proxy-pools/proxy_pools
install -o root -g root -m 0755 "$SCRIPT_DIR/pp" /usr/local/bin/pp
install -o root -g root -m 0755 "$SCRIPT_DIR/proxy-pools.service" /etc/systemd/system/proxy-pools.service
cat > /usr/local/libexec/proxy-pools-start <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
exec /opt/proxy-pools/proxy_pools "${PROXY_SUBSCRIPTION_URL:?PROXY_SUBSCRIPTION_URL is required}"
EOF
chmod 0755 /usr/local/libexec/proxy-pools-start
printf 'PROXY_SUBSCRIPTION_URL=%q\n' "$SUBSCRIPTION_URL" > /etc/proxy-pools/proxy-pools.env
printf '%s\n' "$RELEASE_VERSION" > /etc/proxy-pools/version
chmod 0600 /etc/proxy-pools/proxy-pools.env
chown -R proxy-pools:proxy-pools /var/lib/proxy-pools /home/proxy-pools
systemctl daemon-reload
systemctl enable proxy-pools
if systemctl is-active --quiet proxy-pools; then
  systemctl restart proxy-pools
else
  systemctl start proxy-pools
fi
echo "Installed proxy-pools. Use 'pp status' and 'pp list'."
