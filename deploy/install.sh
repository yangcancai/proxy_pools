#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" -ne 0 ]]; then
  echo "Run as root, for example: sudo bash install.sh 'https://...'" >&2
  exit 1
fi
if [[ $# -ne 1 || -z "$1" ]]; then
  echo "Usage: install.sh <clash-subscription-url>" >&2
  exit 2
fi

SUBSCRIPTION_URL="$1"
REPO="${PROXY_POOLS_REPO:-}"
if [[ -z "$REPO" ]]; then
  echo "Set PROXY_POOLS_REPO=OWNER/REPOSITORY when installing from a GitHub release." >&2
  exit 2
fi

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
ASSET="proxy_pools_linux_${ARCH}"
API="https://api.github.com/repos/${REPO}/releases/latest"
DOWNLOAD_URL="$(curl -fsSL "$API" | python3 -c 'import json,sys; name=sys.argv[1]; data=json.load(sys.stdin); print(next(a["browser_download_url"] for a in data["assets"] if a["name"] == name))' "$ASSET")"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ASSET_DIR=""
if [[ ! -f "$SCRIPT_DIR/pp" || ! -f "$SCRIPT_DIR/proxy-pools.service" ]]; then
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
