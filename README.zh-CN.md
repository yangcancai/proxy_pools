# proxy-pools

[English](README.md)

一个以多米 HTTP 代理 API 为上游的 HTTP 正向代理服务。服务在收到代理请求时获取 `ip:port` 列表，并轮询出口代理。代理池按需刷新，空闲时不会后台调用 API；刷新失败时保留现有列表。

## Clash 订阅模式

直接把 Clash 网络订阅 URL 作为参数，程序会自动下载对应平台的 Mihomo、启动 Mihomo，并为每个节点创建一个本地 `mixed` 端口。端口默认从 `19000` 开始，支持 HTTP、HTTPS `CONNECT` 和 SOCKS5。

```bash
./proxy-pools 'https://example.com/my-clash-subscription.yaml'
```

## 一键安装（Linux）

安装脚本会从 GitHub Release 下载最新 Linux 产物并注册 systemd 服务。目标机器需要安装 `systemd`、`curl` 和 `python3`，并使用 root 权限执行。

将 `OWNER/REPOSITORY` 替换为本项目所在的 GitHub 仓库，将订阅地址替换为自己的地址：

```bash
curl -fsSL https://raw.githubusercontent.com/OWNER/REPOSITORY/main/deploy/install.sh \
  | sudo env PROXY_POOLS_REPO=OWNER/REPOSITORY bash -s -- 'https://example.com/my-clash-subscription.yaml'
```

安装后会创建 `proxy-pools` 系统用户，并安装以下文件：

| 路径 | 用途 |
| --- | --- |
| `/opt/proxy-pools/proxy_pools` | 程序文件 |
| `/etc/proxy-pools/proxy-pools.env` | 订阅地址和 Mihomo 配置 |
| `/etc/proxy-pools/version` | 已安装的 Release 版本 |
| `/etc/systemd/system/proxy-pools.service` | systemd 服务 |
| `/usr/local/bin/pp` | 服务管理命令 |

发布新版本后重复执行安装命令即可升级，程序会替换二进制并重启服务。订阅地址会保存在环境文件中，如果 URL 包含凭据，请注意保护该文件。

安装后使用 `pp`：

```text
pp start       启动
pp stop        停止
pp restart     重启
pp status      查看状态
pp list        查看节点、端口和 SOCKS5 地址
pp log         查看实时日志
pp version     查看版本
pp help        查看帮助和项目广告
```

默认 Mihomo Controller 地址为 `http://127.0.0.1:9090`，可以在 `/etc/proxy-pools/proxy-pools.env` 中配置 `MIHOMO_CONTROLLER`、`MIHOMO_SECRET` 和 `MIHOMO_PORT_START`，然后执行 `pp restart`。

例如：

```bash
sudo sh -c 'cat >> /etc/proxy-pools/proxy-pools.env <<EOF
MIHOMO_SECRET=your-secret
MIHOMO_PORT_START=20000
EOF'
sudo pp restart
sudo pp list
curl -x http://127.0.0.1:20000 https://ifconfig.me
```

如需手动卸载服务：

```bash
sudo systemctl disable --now proxy-pools
sudo rm -f /etc/systemd/system/proxy-pools.service /usr/local/bin/pp /usr/local/libexec/proxy-pools-start
sudo systemctl daemon-reload
```

## 多米 API 模式

```bash
export PROXY_API_URL='http://api.dmdaili.com/dmgetip.asp?apikey=你的值&pwd=你的值&getnum=10&httptype=1&geshi=1&fenge=1&Contenttype=1&operate=all'
go run ./cmd/proxy-pools
```

默认监听 `127.0.0.1:18080`：

```bash
curl -x http://127.0.0.1:18080 https://httpbin.org/ip
```

## 配置

| 环境变量 | 启动参数 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `PROXY_API_URL` | `-api` | 必填 | 多米代理 API 地址 |
| `PROXY_LISTEN` | `-listen` | `127.0.0.1:18080` | 监听地址 |
| `PROXY_REFRESH_INTERVAL` | `-refresh` | `20s` | 按需刷新间隔 |
| `PROXY_FETCH_TIMEOUT` | `-fetch-timeout` | `10s` | API 请求超时 |
| `PROXY_DIAL_TIMEOUT` | `-dial-timeout` | `5s` | 上游连接超时 |
| `PROXY_SHUTDOWN_TIMEOUT` | `-shutdown-timeout` | `10s` | 优雅退出超时 |

## 构建

```bash
go test ./...
go build -o proxy-pools ./cmd/proxy-pools
```

推送 `v*` tag 后，GitHub Actions 会自动构建 Linux amd64/arm64 产物并发布到 GitHub Release。
