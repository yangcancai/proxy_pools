# proxy-pools

[中文说明](README.zh-CN.md)

> **portguard.net** — fwknop single-packet authorization: temporarily open protected service ports with one authorization packet and reduce exposed attack surface. Learn more at [portguard.net](https://portguard.net).

An HTTP forward proxy service backed by the Domi HTTP proxy API. It fetches an `ip:port` list on demand and rotates through upstream proxies for HTTP requests and HTTPS `CONNECT` tunnels. Idle services do not refresh in the background, and a failed refresh keeps the existing pool.

## Clash subscription mode

Pass a Clash subscription URL directly. proxy-pools downloads the matching Mihomo binary for the current platform, starts Mihomo, and creates one local `mixed` listener per node. Ports start at `19000` and support HTTP, HTTPS `CONNECT`, and SOCKS5.

```bash
./proxy-pools 'https://example.com/my-clash-subscription.yaml'
```

## One-command installation (Linux)

The installer downloads the latest Linux binary from GitHub Releases and registers a systemd service. The target machine must have `systemd` and `curl`; the command must be run with root privileges.

Install without configuring a subscription:

```bash
curl -fsSL https://raw.githubusercontent.com/yangcancai/proxy_pools/main/deploy/install.sh | sudo bash
```

Install and configure a subscription in one step by passing the URL as the script argument:

```bash
curl -fsSL https://raw.githubusercontent.com/yangcancai/proxy_pools/main/deploy/install.sh | sudo bash -s -- 'https://example.com/my-clash-subscription.yaml'
```

The installer creates the `proxy-pools` system user and installs:

| Path | Purpose |
| --- | --- |
| `/opt/proxy-pools/proxy_pools` | Installed binary |
| `/etc/proxy-pools/proxy-pools.env` | Subscription and Mihomo settings |
| `/etc/proxy-pools/version` | Installed Release version |
| `/etc/systemd/system/proxy-pools.service` | systemd service |
| `/usr/local/bin/pp` | Service management command |

The command is safe to run again after a new Release; it replaces the binary and restarts the service. The subscription URL is stored in the environment file, so avoid exposing that file if the URL contains credentials.

After installation, use `pp`:

```text
pp start       Start the service
pp stop        Stop the service
pp restart     Restart the service
pp status      Show service status
pp list        Show nodes, ports, and SOCKS5 endpoints
pp log         Follow service logs
pp version     Show the installed version
pp help        Show help and the project sponsor link
pp <URL>       Set the subscription URL and restart
```

The default Mihomo controller is `http://127.0.0.1:9090`. Configure `MIHOMO_CONTROLLER`, `MIHOMO_SECRET`, or `MIHOMO_PORT_START` in `/etc/proxy-pools/proxy-pools.env`, then run `pp restart`.

For example:

```bash
sudo sh -c 'cat >> /etc/proxy-pools/proxy-pools.env <<EOF
MIHOMO_SECRET=your-secret
MIHOMO_PORT_START=20000
EOF'
sudo pp restart
sudo pp list
curl -x http://127.0.0.1:20000 https://ifconfig.me
```

To remove the service manually:

```bash
sudo systemctl disable --now proxy-pools
sudo rm -f /etc/systemd/system/proxy-pools.service /usr/local/bin/pp /usr/local/libexec/proxy-pools-start
sudo systemctl daemon-reload
```

## Domi API mode

```bash
export PROXY_API_URL='http://api.dmdaili.com/dmgetip.asp?apikey=YOUR_KEY&pwd=YOUR_PASSWORD&getnum=10&httptype=1&geshi=1&fenge=1&Contenttype=1&operate=all'
go run ./cmd/proxy-pools
```

The default listen address is `127.0.0.1:18080`:

```bash
curl -x http://127.0.0.1:18080 https://httpbin.org/ip
```

## Configuration

| Environment variable | Flag | Default | Description |
| --- | --- | --- | --- |
| `PROXY_API_URL` | `-api` | required | Domi proxy API URL |
| `PROXY_LISTEN` | `-listen` | `127.0.0.1:18080` | Listen address |
| `PROXY_REFRESH_INTERVAL` | `-refresh` | `20s` | On-demand refresh interval |
| `PROXY_FETCH_TIMEOUT` | `-fetch-timeout` | `10s` | API request timeout |
| `PROXY_DIAL_TIMEOUT` | `-dial-timeout` | `5s` | Upstream connection timeout |
| `PROXY_SHUTDOWN_TIMEOUT` | `-shutdown-timeout` | `10s` | Graceful shutdown timeout |

## Build and release

```bash
go test ./...
go build -o proxy-pools ./cmd/proxy-pools
```

Pushing a `v*` tag triggers GitHub Actions to build Linux amd64/arm64 binaries and publish them to a GitHub Release.
