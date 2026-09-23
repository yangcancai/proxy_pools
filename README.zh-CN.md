# proxy-pools

[English](README.md)

> **portguard.net** — fwknop 单包授权工具：用一个授权数据包临时开启受保护服务端口，减少暴露面。了解更多：[portguard.net](https://portguard.net)。

一个以多米 HTTP 代理 API 为上游的 HTTP 正向代理服务。服务在收到代理请求时获取 `ip:port` 列表，并轮询出口代理。代理池按需刷新，空闲时不会后台调用 API；刷新失败时保留现有列表。

## Clash 订阅模式

直接把 Clash 网络订阅 URL 作为参数，程序会自动下载对应平台的 Mihomo、启动 Mihomo，并为每个节点创建一个本地 `mixed` 端口。端口默认从 `19000` 开始，支持 HTTP、HTTPS `CONNECT` 和 SOCKS5。

```bash
./proxy-pools 'https://example.com/my-clash-subscription.yaml'
```

## 一键安装（Linux）

安装脚本会从 GitHub Release 下载最新 Linux 产物并注册 systemd 服务。目标机器需要安装 `systemd`、`curl` 和 `openssl`，并使用 root 权限执行。

只安装程序，不设置订阅：

```bash
curl -fsSL https://raw.githubusercontent.com/yangcancai/proxy_pools/main/deploy/install.sh | sudo bash
```

安装并立即配置订阅：

```bash
curl -fsSL https://raw.githubusercontent.com/yangcancai/proxy_pools/main/deploy/install.sh | sudo bash -s -- 'https://example.com/my-clash-subscription.yaml'
```

安装后会创建 `proxy-pools` 系统用户，并安装以下文件：

| 路径 | 用途 |
| --- | --- |
| `/opt/proxy-pools/proxy_pools` | 程序文件 |
| `/etc/proxy-pools/proxy-pools.env` | 订阅地址和 Mihomo 配置 |
| `/etc/proxy-pools/subscriptions.key` | 订阅加密密钥 |
| `/var/lib/proxy-pools/subscriptions.enc` | 加密后的订阅列表 |
| `/var/lib/proxy-pools/listeners.tsv` | 当前节点和 SOCKS5 端口列表 |
| `/etc/proxy-pools/version` | 已安装的 Release 版本 |
| `/etc/systemd/system/proxy-pools.service` | systemd 服务 |
| `/usr/local/bin/pp` | 服务管理命令 |

发布新版本后重复执行安装命令即可升级，程序会替换二进制并重启服务。订阅地址会加密保存在 `/var/lib/proxy-pools/subscriptions.enc`，环境文件只保存运行配置。

安装后使用 `pp`：

```text
pp start       启动
pp stop        停止
pp restart     重启
pp status      查看状态
pp list        查看节点、端口和 SOCKS5 地址
pp listen IP   修改监听地址并重启
pp allowlist    查看客户端 IP 白名单
pp allowlist IPs  设置逗号分隔的 IP/CIDR 并重启
pp reset-auth  重置所有 listener 密码（用户名按节点固定生成）
pp reset-password  reset-auth 的别名
pp export      导出当前节点 JSON
pp export quick [--protocol socks|http|https] 每行导出一个代理地址
pp subscriptions  查看当前订阅列表
pp add URL      添加订阅并重启
pp remove N     删除订阅并重启
pp update       立即更新订阅
pp log         查看实时日志
pp version     查看版本
pp help        查看帮助和项目广告
pp <URL>       设置订阅地址并重启
```

多个订阅会按节点名称合并，重名节点保留第一个订阅中的版本。订阅地址会加密保存；节点和 SOCKS5 端口映射会持久化，重启或重新安装后会尽量复用已有端口。

默认 Mihomo Controller 地址为 `http://127.0.0.1:9090`，可以在 `/etc/proxy-pools/proxy-pools.env` 中配置 `MIHOMO_CONTROLLER`、`MIHOMO_SECRET` 和 `MIHOMO_PORT_START`，然后执行 `pp restart`。

订阅默认每小时自动更新。可以设置 `MIHOMO_SUBSCRIPTION_REFRESH=30m` 修改间隔，或执行 `sudo pp update` 立即更新。

监听地址默认是 `127.0.0.1`。如需监听所有网卡，可以执行 `sudo pp listen 0.0.0.0`；也可以设置为指定服务器 IP。使用 `sudo pp export /tmp/proxies.json` 导出当前合并后的节点。导出内容包含代理凭据，请妥善保护导出文件。

如需快速导入截图中的代理列表工具，可以指定客户端可访问的地址：

```bash
sudo pp export quick --host 192.168.1.20 /tmp/proxies.txt
sudo pp export quick --protocol http --host 192.168.1.20
sudo pp export quick --protocol https --host 192.168.1.20
sudo pp export quick --docker /tmp/proxies.txt
sudo pp export quick --docker
```

默认协议是 `socks5`，也可以使用 `socks`、`http` 或 `https` 选择导出的 URL 协议。`--docker` 会使用 `host.docker.internal`。导出结果每行一个带认证的地址，例如 `socks5://user:pass@192.168.1.20:19000`。

可以使用 `sudo pp allowlist 203.0.113.10,10.0.0.0/8` 设置客户端 IP 白名单；使用 `sudo pp allowlist off` 关闭。其他来源地址会在代理转发前被拒绝。

每个自动生成的本地 listener 都会自动分配随机用户名和密码，并持久化到 `/var/lib/proxy-pools/auth.tsv`。`pp list` 会显示带认证信息的 SOCKS5 地址，例如 `socks5://pp_xxx:password@127.0.0.1:19000`。

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
