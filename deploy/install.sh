#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" -ne 0 ]]; then
  echo "Run as root, for example: sudo bash install.sh [subscription-url]" >&2
  exit 1
fi
if ! command -v openssl >/dev/null 2>&1; then
  echo "openssl is required" >&2
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
if [[ ! -s /etc/proxy-pools/subscriptions.key ]]; then
  openssl rand -hex 32 > /etc/proxy-pools/subscriptions.key
fi
chown root:proxy-pools /etc/proxy-pools/subscriptions.key
chmod 0640 /etc/proxy-pools/subscriptions.key
tmp="$(mktemp)"
trap 'rm -f "${tmp:-}"; if [[ -n "${ASSET_DIR:-}" ]]; then rm -rf "$ASSET_DIR"; fi' EXIT
curl -fL --retry 3 -o "$tmp" "$DOWNLOAD_URL"
install -o root -g root -m 0755 "$tmp" /opt/proxy-pools/proxy_pools
install -o root -g root -m 0755 "$SCRIPT_DIR/pp" /usr/local/bin/pp
install -o root -g root -m 0755 "$SCRIPT_DIR/proxy-pools.service" /etc/systemd/system/proxy-pools.service
cat > /usr/local/libexec/proxy-pools-start <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
SUBSCRIPTIONS_FILE=/var/lib/proxy-pools/subscriptions.enc
SUBSCRIPTIONS_KEY=/etc/proxy-pools/subscriptions.key
RUNTIME_SUBSCRIPTIONS=/run/proxy-pools/subscriptions
if [[ ! -s "$SUBSCRIPTIONS_FILE" ]]; then
  echo "No encrypted subscriptions configured; run: sudo pp add 'https://...'" >&2
  exit 1
fi
openssl enc -d -aes-256-cbc -pbkdf2 -iter 600000 -pass file:"$SUBSCRIPTIONS_KEY" \
  -in "$SUBSCRIPTIONS_FILE" -out "$RUNTIME_SUBSCRIPTIONS"
chmod 0600 "$RUNTIME_SUBSCRIPTIONS"
trap 'rm -f "$RUNTIME_SUBSCRIPTIONS"' EXIT
exec /opt/proxy-pools/proxy_pools
EOF
chmod 0755 /usr/local/libexec/proxy-pools-start
if [[ -n "$SUBSCRIPTION_URL" ]]; then
  printf '%s\n' "$SUBSCRIPTION_URL" | openssl enc -aes-256-cbc -salt -pbkdf2 -iter 600000 \
    -pass file:/etc/proxy-pools/subscriptions.key > /var/lib/proxy-pools/subscriptions.enc
  chown proxy-pools:proxy-pools /var/lib/proxy-pools/subscriptions.enc
  chmod 0600 /var/lib/proxy-pools/subscriptions.enc
fi
if [[ -f /etc/proxy-pools/proxy-pools.env ]]; then
  sed '/^PROXY_SUBSCRIPTION_URL=/d' /etc/proxy-pools/proxy-pools.env > /etc/proxy-pools/proxy-pools.env.tmp
  mv /etc/proxy-pools/proxy-pools.env.tmp /etc/proxy-pools/proxy-pools.env
else
  : > /etc/proxy-pools/proxy-pools.env
fi
printf '%s\n' "$RELEASE_VERSION" > /etc/proxy-pools/version
chmod 0600 /etc/proxy-pools/proxy-pools.env
chown -R proxy-pools:proxy-pools /var/lib/proxy-pools /home/proxy-pools
systemctl daemon-reload
systemctl enable proxy-pools
if [[ -s /var/lib/proxy-pools/subscriptions.enc ]]; then
  if systemctl is-active --quiet proxy-pools; then
    systemctl restart proxy-pools
  else
    systemctl start proxy-pools
  fi
else
  echo "Installed without subscriptions. Run: sudo pp add 'https://...'"
fi
echo "Installed proxy-pools. Use 'pp status' and 'pp list'."
