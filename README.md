# proxy-pools

一个以多米 HTTP 代理 API 为上游的 HTTP 正向代理服务。服务在收到第一个代理请求时获取 API 返回的 `ip:port` 列表，并对每个 HTTP 请求或 HTTPS `CONNECT` 隧道按顺序轮询出口代理。代理池按需刷新：没有代理请求时不会调用 API；有请求且距上次获取已达到刷新间隔时，先更新代理池再转发。刷新失败时保留现有列表。无请求体的 GET/HEAD 和尚未建立的 HTTPS 隧道遇到坏节点时，最多自动尝试 3 个出口；可能产生副作用的请求不会被自动重放。

## 启动

准备多米网站生成的 API 链接，建议让接口按一行一个 `ip:port` 返回多个地址（示例代码中的 `getnum=10&httptype=1&geshi=1&fenge=1&Contenttype=1` 格式可直接解析）：

```bash
export PROXY_API_URL='http://api.dmdaili.com/dmgetip.asp?apikey=你的值&pwd=你的值&getnum=10&httptype=1&geshi=1&fenge=1&fengefu=&Contenttype=1&operate=all'
go run ./cmd/proxy-pools
```

默认只监听 `127.0.0.1:18080`。针对 30～60 秒存活期的出口 IP，默认允许在上次 API 调用完成 20 秒后的下一次代理请求中重新获取代理池。空闲期间没有后台刷新；空闲很久后的第一个请求会等待一次 API 获取。遇到 API 限流时依次退避到 40 秒、80 秒，最长 2 分钟，连续成功后逐级恢复。测试代理：

```bash
curl -x http://127.0.0.1:18080 http://httpbin.org/ip
curl -x http://127.0.0.1:18080 https://httpbin.org/ip
```

每执行一次请求就会选择池中的下一个代理；HTTPS 的一次 `CONNECT` 隧道在整个连接期间固定使用同一个出口。

每次出口尝试都会打印请求 URL、出口代理 IP 和端口。例如：

```text
2026/09/02 10:30:00 proxy-pools forward method=GET url=http://httpbin.org/ip proxy=49.87.198.137:15001 attempt=1/3
2026/09/02 10:30:01 proxy-pools forward method=CONNECT url=https://httpbin.org:443 proxy=223.167.143.26:15001 attempt=1/3
```

每次实际调用代理列表 API 时也会记录脱敏后的接口 URL、响应状态、耗时和 `Retry-After`。`apikey`、`pwd`、`token` 等凭据不会写入日志：

```text
2026/09/02 15:32:56 proxy-pools proxy API request method=GET url=http://api.dmdaili.com/dmgetip.asp?apikey=%2A%2A%2A&pwd=%2A%2A%2A&getnum=2
2026/09/02 15:32:56 proxy-pools proxy API response url=http://api.dmdaili.com/dmgetip.asp?apikey=%2A%2A%2A&pwd=%2A%2A%2A&getnum=2 status=200 result=rate_limited duration=120ms bytes=67 retry_after=-
```

## 配置

| 环境变量 | 启动参数 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `PROXY_API_URL` | `-api` | 必填 | 多米代理 API 完整链接 |
| `PROXY_LISTEN` | `-listen` | `127.0.0.1:18080` | 服务监听地址；如需对外提供可改为 `:18080`，并在外围加访问控制 |
| `PROXY_REFRESH_INTERVAL` | `-refresh` | `20s` | 有代理请求时的按需刷新间隔；空闲时不拉取，API 调用间隔不会短于 20 秒 |
| `PROXY_FETCH_TIMEOUT` | `-fetch-timeout` | `10s` | API 请求超时 |
| `PROXY_DIAL_TIMEOUT` | `-dial-timeout` | `5s` | 连接上游代理及等待响应头的超时 |
| `PROXY_SHUTDOWN_TIMEOUT` | `-shutdown-timeout` | `10s` | 优雅退出超时 |
| `PROXY_UPSTREAM_USERNAME` | `-upstream-username` | 空 | 上游代理 Basic Auth 用户名（如需要） |
| `PROXY_UPSTREAM_PASSWORD` | `-upstream-password` | 空 | 上游代理 Basic Auth 密码（如需要） |

API 链接通常包含密钥，生产环境应优先通过环境变量或 Secret 注入，不要提交到仓库。

## 构建

```bash
go test ./...
go build -o proxy-pools ./cmd/proxy-pools
```

## Clash YAML 节点端口管理

管理模式会自动从 Mihomo GitHub Releases 下载当前系统和架构对应的预编译程序，启动并管理 Mihomo 的生命周期，然后下载网络订阅，为每个节点创建一个本地 `mixed` 端口。`mixed` 同时接受 HTTP、HTTPS `CONNECT` 和 SOCKS5 请求；端口按 YAML 中 `proxies` 的顺序从 `19000` 开始分配。

```bash
./proxy-pools 'https://example.com/my-clash-subscription.yaml'
```

默认调用 `http://127.0.0.1:9090`；如有需要再通过 `MIHOMO_CONTROLLER`、`MIHOMO_SECRET`、`MIHOMO_PORT_START` 覆盖。Mihomo 缓存在系统用户缓存目录下，后续启动会复用已下载的程序。按 Ctrl-C 会停止 Mihomo。

例如节点名为 `香港-01` 时，会创建一个指向该节点的 Mihomo listener；日志会打印实际端口。之后可以这样使用：

```bash
curl -x http://127.0.0.1:19000 https://example.com
curl --socks5-hostname 127.0.0.1:19000 https://example.com
```

管理模式会在启动 Mihomo 前把 `listeners` 写入运行配置，由 Mihomo 直接创建端口；不会依赖动态 listener API。默认 listener 名称格式为 `proxy-pools-节点名`，可用 `-listener-prefix` 修改。原有的多米 API 模式仍按上面的启动方式工作。
