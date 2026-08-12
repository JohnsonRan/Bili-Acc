# Bili-Acc

Bili-Acc 是使用 Go 编写的 B 站固定出口媒体代理。它将 playurl API、点播视频与音频、直播 HLS 播放列表和媒体分片统一转发到同一台 Linux VPS，并由浏览器用户脚本或 Surge 模块改写播放地址。

本文档以当前 `main` 分支代码为准。项目不会绕过账号、地区或内容本身的授权限制；最终可用画质仍由 B 站上游和登录账号决定。

## 特性

### 🚀 流式媒体代理

- **固定出口** — playurl API 和媒体请求使用同一台 VPS 的公网出口。
- **流式转发** — 普通媒体通过固定大小缓冲区直接转发，不会完整读入内存。
- **Range / 206** — 透传 `Range`、`If-Range` 等请求头，支持拖动进度和断点读取。
- **HLS 改写** — 自动识别 M3U8，改写分片、子播放列表、密钥和标签中的 `URI`。
- **连接复用** — 上游客户端启用 HTTP/2、连接池和明确的连接阶段超时。
- **固定 IPv4** — 上游连接强制使用 IPv4，并忽略环境代理变量，避免出口发生偏移。

### 🔧 客户端接入

- **Tampermonkey 用户脚本** — 拦截网页端 `fetch` 和 `XMLHttpRequest` playurl 请求，并改写返回的媒体 URL。
- **Surge 模块** — 支持 Surge iOS 5.9+ 和 Surge Mac 5.5+，通过请求与响应脚本完成同样的代理改写。
- **点播与直播** — 覆盖网页点播、番剧、直播新版和直播旧版 playurl API。
- **登录态转发** — 临时转发 B 站 Cookie，以保留账号可用的清晰度和内容权限。
- **最高可用画质** — 服务端请求账号当前可用的最高画质，失败时回退到原始请求。

### 🛡️ 访问控制

- **路径 Token** — `/playurl/` 和 `/proxy/` 请求都必须携带匹配的私有 Token。
- **严格 API 白名单** — playurl 仅允许访问已知的 B 站 API 主机和路径。
- **媒体域名白名单** — 媒体请求及重定向只能访问配置的域名后缀。
- **安全重定向** — 最多跟随 5 次重定向，并在每一跳重新检查目标主机。
- **受限 CORS** — 仅允许 B 站 HTTPS 页面来源，或没有浏览器 Origin 的直接请求。
- **敏感信息隔离** — Cookie 只随 playurl 上游请求转发，不写入应用日志或磁盘。

### 📊 运维与诊断

- **聚合式可观测性** — `slog` 错误去重、周期流量汇总和同端口只读诊断页替代成功媒体请求刷屏，同时保持敏感信息脱敏。
- **优雅关闭** — 收到 `SIGTERM` 或中断信号后，最多等待 30 秒完成已有请求。
- **Go Flight Recorder** — Unix 环境可在内存中保留近期运行时事件，并通过 `SIGUSR1` 写出 trace。
- **容器部署** — 提供多阶段 `Dockerfile` 和仅绑定回环地址的 Docker Compose 配置。
- **外部 TLS** — 由宿主机 Caddy 提供 HTTPS、HTTP/2 和可选的 HTTP/3。

## 当前实现

- `GET /` 健康检查。
- Token 保护的 `/playurl/` 网页 playurl API 转发。
- Token 保护的 `/playurl-grpc/` 原生 App `Player/PlayViewUnite` gRPC 转发。
- Token 保护的 `/proxy/` 点播视频、音频、直播媒体和 HLS 转发。
- `/playurl/` 和 `/proxy/` 仅支持 `GET`、`HEAD`；`/playurl-grpc/` 仅支持 `POST`；同时支持 CORS 预检 `OPTIONS`。
- 支持以下网页 playurl API：
  - `/x/player/playurl`
  - `/x/player/wbi/playurl`
  - `/pgc/player/web/playurl`
  - `/pgc/player/web/v2/playurl`
  - `/xlive/web-room/v2/index/getRoomPlayInfo`
  - `/room/v1/Room/playUrl`
- WBI 请求重新签名，并缓存 WBI mixin key 一小时。
- 视频请求使用 `qn=127`、`fnval=4048` 和 `fourk=1` 请求最高可用画质。
- 直播新版请求使用 `qn=10000`，旧版请求使用 `quality=4`。
- HLS playlist 最大读取 4 MiB，读取和改写阶段最长 30 秒。
- 普通媒体不设置全局响应超时，避免长视频流被固定时限中断。
- 媒体响应不缓存；Range 请求和上游 `206 Partial Content` 按原状态流式返回。

最高画质参数只表达请求意图，不会突破账号等级、付费状态、地区限制或上游实际可用的清晰度。playurl 响应中的画质列表和元数据由上游返回，客户端仍可以正常降级。

## 快速开始

### 部署要求

- 一台具有固定公网 IPv4 的 Linux VPS。
- Docker 和 Docker Compose。
- 安装在宿主机上的 Caddy。
- 一个解析到 VPS 的域名。
- 对外开放 TCP 80/443；需要 HTTP/3 时同时开放 UDP 443。

本地构建和检查使用 Go 1.26。运行 `go test -race` 还需要 GCC 或 Clang。

### Docker Compose

```bash
git clone https://github.com/JohnsonRan/Bili-Acc.git
cd Bili-Acc
cp .env.example .env
```

编辑 `.env`：

```dotenv
DOMAIN=bili.example.com
TOKEN=replace-with-a-long-random-token
LOG_FORMAT=text
LOG_LEVEL=info
LOG_MEDIA_SUCCESS=false
LOG_SUMMARY_INTERVAL=60s
LOG_ERROR_DEDUP_INTERVAL=10s
LOG_CLIENT_IP=masked
```

生成 Token 时应使用足够长的随机值，例如：

```bash
openssl rand -hex 32
```

启动服务：

```bash
docker compose up -d --build
docker compose logs -f app
```

Compose 只将应用发布到 `127.0.0.1:8080`。代理接口和只读诊断页共用该监听端口，由宿主机 Caddy 统一提供 HTTPS。

### 宿主机 Caddy

在宿主机 Caddyfile 中添加：

```caddyfile
bili.example.com {
	reverse_proxy 127.0.0.1:8080 {
		flush_interval -1
	}
}
```

`flush_interval -1` 用于及时向客户端转发流式响应。重新加载 Caddy 后检查：

```bash
curl https://bili.example.com/
```

正常响应：

```text
Bili CF Acc is running
```

### 更新

```bash
git pull
docker compose up -d --build
docker image prune -f
```

## 请求路径

### 健康检查

```text
GET /
```

返回纯文本 `Bili CF Acc is running`。该路径不需要 Token，可用于 Caddy 或外部监控探活。

### playurl 代理

```text
/playurl/{token}/{base64url(origin)}/{path}?{query}
```

示例目标：

```text
https://api.bilibili.com/x/player/wbi/playurl?bvid=...&cid=...
```

其中：

- `token` 是 URL path 编码后的私有 Token；
- `base64url(origin)` 是不带填充的 URL-safe Base64，例如 `https://api.bilibili.com`；
- `path` 和查询参数按请求保留；
- 目标必须是 HTTPS、不能携带用户信息或显式端口，并且必须匹配 playurl 白名单。

服务只向上游复制必要的 `Accept`、`Accept-Language` 和 `User-Agent`。客户端可通过 `X-Bili-Cookie` 临时传递 Cookie，通过 `X-Bili-Referer` 传递页面来源。

### 原生 gRPC playurl 代理

```text
POST /playurl-grpc/{token}/{base64url(origin)}/bilibili.app.playerunite.v1.Player/PlayViewUnite
```

该路由只允许 `grpc.biliapi.net`、`app.bilibili.com` 和 `app.biliapi.net` 的上述精确方法，完整转发最多 2 MiB 的 protobuf 请求体及必要的 gRPC metadata，并强制上游使用 `grpc-accept-encoding: identity`。Surge 到 Bili-Acc 的外层请求使用 `application/x-bili-acc-grpc` 普通二进制 HTTP 隧道，后端再恢复为上游 `application/grpc`，避免 Cloudflare/Caddy 将该请求误判为需要端到端 HTTP/2 gRPC 的连接并返回 HTML 错误页。响应体最多缓冲 8 MiB，以便在读取上游 trailer 后同时保留真实 gRPC trailer，并通过内部普通响应头镜像经过校验的 `grpc-status`；Surge 会移除内部头并恢复 `Grpc-Status`，因此外层 CDN 即使不保留普通 HTTP trailer，也不会丢失 RPC 状态。其他主机、路径或 HTTP 方法不会转发。

原生 App 必须先通过此路由获取播放地址，使媒体 URL 的来源 IP 签名绑定到 VPS 出口；随后 `/proxy/` 才能使用同一出口拉取媒体，避免手机直取 playurl、VPS 拉媒体造成的 IP 签名不一致和上游 `403`。

### 媒体代理

```text
/proxy/{token}/{base64url(origin)}/{path}?{query}
```

媒体目标可以使用 HTTP 或 HTTPS，但主机及所有重定向目标都必须匹配 `ALLOWED_HOSTS`。服务会复制以下请求头：

- `Accept`
- `If-Modified-Since`
- `If-None-Match`
- `If-Range`
- `Range`
- `User-Agent`

服务统一设置 `Referer: https://www.bilibili.com/`。普通媒体响应保持上游状态码和必要响应头；M3U8 响应会移除长度、Range、摘要和缓存校验相关头，再返回改写后的完整 playlist。

### HLS 行为

服务根据响应 `Content-Type` 或 `.m3u8` 路径识别 HLS playlist，并改写：

- 普通 URI 行；
- `URI="..."` 标签属性；
- 相对 URL 和绝对 URL；
- 子播放列表、媒体分片和密钥 URL。

playlist 中出现不在媒体白名单内的目标时，整个请求返回 `502`。上游若对 playlist 返回 `206`，服务同样返回 `502`，因为局部 playlist 无法安全改写。

## 配置

配置通过环境变量传入：

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `TOKEN` | 无，必填 | 私有代理令牌，同时配置到用户脚本或 Surge 模块。 |
| `LISTEN_ADDR` | `:8080` | Go HTTP 服务监听地址。Compose 使用容器内默认值。 |
| `PUBLIC_URL` | 从请求推断 | HLS 改写生成的公开服务地址。Compose 设置为 `https://${DOMAIN}`。 |
| `ALLOWED_HOSTS` | B 站媒体域名 | 逗号分隔的媒体域名后缀白名单。 |
| `LOG_FORMAT` | `text` | 日志格式，可选 `text` 或 `json`。 |
| `LOG_LEVEL` | `info` | `debug`、`info`、`warn` 或 `error`。 |
| `LOG_MEDIA_SUCCESS` | `false` | 是否逐条记录成功的媒体 `200/206` 请求。默认关闭以避免 Range 请求刷屏。 |
| `LOG_SUMMARY_INTERVAL` | `60s` | 流量汇总周期，设为 `0` 关闭；非零值不得低于 `10s`。空闲周期不会输出。 |
| `LOG_ERROR_DEDUP_INTERVAL` | `10s` | 相同安全错误键的日志去重周期，设为 `0` 关闭；非零值不得低于 `1s`。 |
| `LOG_CLIENT_IP` | `masked` | 客户端 IP 日志策略：`full`、`masked` 或 `off`。输入必须能解析为 IP，否则记录 `unknown`；诊断页始终不展示 IP。 |
| `TRACE_DIR` | 空 | 非空时在 Unix 环境启用 Flight Recorder，并将 trace 写入该目录。 |

默认媒体域名后缀：

```text
bilivideo.com,bilivideo.cn,biliapi.net,akamaized.net
```

`ALLOWED_HOSTS` 使用后缀匹配。例如配置 `bilivideo.com` 时允许 `bilivideo.com` 和 `*.bilivideo.com`，但不会允许名称仅以该字符串结尾的无关域名。

### Compose 配置

当前 `compose.yaml`：

- 将 `TOKEN` 从 `.env` 注入容器；
- 使用 `DOMAIN` 生成 `PUBLIC_URL`；
- 将应用发布到 `127.0.0.1:8080`，代理与 `/diagnostics/` 只读诊断页共用该端口；
- 将 `/traces` 挂载到 Docker 命名卷；
- 使用 `unless-stopped` 自动重启策略；
- 为已有媒体请求保留 40 秒容器停止宽限期。

若修改宿主机回环端口，需要同步更新 `compose.yaml` 和 Caddyfile。不要将应用端口直接绑定到 `0.0.0.0`。诊断页只展示脱敏、只读的运行指标，但没有内置登录功能；如果不希望公开 `/diagnostics/`，应由宿主机 Caddy 或 Cloudflare Access 对该路径增加认证或访问限制。

## 客户端

### Tampermonkey / Violentmonkey 用户脚本

编辑 `userscript/bili-cf-acc.user.js`：

```js
const SERVER = "https://bili.example.com";
const TOKEN = "replace-with-the-same-token-as-the-server";
```

随后：

1. 在 Tampermonkey 或 Violentmonkey 中新建脚本并粘贴文件内容，或从本地文件导入。
2. 确认脚本在 `www.bilibili.com` 和 `live.bilibili.com` 上启用。
3. 授权 `GM_cookie`，以读取包括 HttpOnly 在内的 B 站登录 Cookie。
4. 刷新 B 站页面并开始播放。

脚本会在 `document-start` 阶段拦截网页端 GET playurl 请求，将 Cookie 放入 `X-Bili-Cookie`，并改写 fetch JSON/text、XHR response/responseText 以及页面初始 `__playinfo__`/直播状态中的媒体 URL，避免首次打开后必须切换清晰度才生效。Cookie 在脚本内缓存 15 秒，不会由服务端持久化。

### Surge

Surge iOS 5.9+ 或 Surge Mac 5.5+ 可以直接安装参数化模块：

```text
https://raw.githubusercontent.com/JohnsonRan/Bili-Acc/main/surge/bili-acc.sgmodule
```

安装时填写：

- `server`：公开 HTTPS 服务地址，例如 `https://bili.example.com`；
- `token`：与服务端 `TOKEN` 完全一致的值。

Surge 还需要：

- 启用 MITM；
- 安装并信任 Surge CA；
- 允许模块 MITM playurl API 和媒体 CDN 域名。

网页端请求仍通过 `/playurl/` 响应改写。原生 App 的 `Player/PlayViewUnite` POST 请求会先改写到 Bili-Acc `/playurl-grpc/`，让 B 站按 VPS 出口生成媒体签名；随后 `bilivideo.com`、`bilivideo.cn` 或 `biliapi.net` 媒体请求改写为 `/proxy/` 地址，并保留 `Range` 等流式请求头。作为服务端兜底，`/proxy/` 在实际请求上游前也会将海外 COS/Huawei/Ali/AWS/Akamai 兼容组统一规范为 `upos-sz-mirrorali.bilivideo.com`，因此客户端即使仍提交 `mirrorcosov` 或 `mirrorhwov` URL，VPS 也不会直接访问这些节点。

为避免对共享 CDN `*.akamaized.net` 做全域 MITM，模块只 MITM `grpc.biliapi.net`、`app.bilibili.com` 和 `app.biliapi.net` 的 `Player/PlayViewUnite` 原生播放请求与响应。请求脚本以 binary body mode 保存 protobuf 请求体，通过普通二进制 HTTP 隧道转发到 VPS，并声明 `grpc-accept-encoding: identity`。响应脚本按 PlayerUnite protobuf 字段结构处理视频、普通音频、Dolby、无损音频和分段流 URL，并参考 BiliUniverse Redirect 的 OverseaVideo 兼容分组，将海外 COS/Huawei/Ali/AWS/Akamai 地址统一规范到 Ali CDN 后包装为 Bili-Acc `/proxy/` URL；如果上游仍返回 gzip message frame，脚本会先解压、改写并重新输出未压缩 frame。未知压缩算法或没有 B 站备用地址时保持原样。主模块完全不声明 `akamaized.net` MITM。

若模块没有生效，在 Surge 的脚本日志或请求备注中搜索 `[Bili Acc]`；模块已启用 `debug=true`，因此命中脚本的 `console.log()` 会同时写入请求备注。更新模块后应确认请求日志包含 `tunnel=http`；如果仍只有 `compression=identity`，说明 Surge 仍在使用旧版远程脚本，应删除旧模块后从原始模块 URL 重新安装。模块的 `script-path` 带版本参数，用于在发布修复时绕过 Surge 的远程脚本缓存。一次正常的点播请求至少应看到：

```text
[Bili Acc][request] rewrite api_host=api.bilibili.com api_path=/x/player/wbi/playurl cookie_present=true
[Bili Acc][response] rewrite status=200 source=proxied_api media_urls=4
[Bili Acc][grpc-request] rewrite api_host=grpc.biliapi.net compression=identity tunnel=http
[Bili Acc][grpc-response] rewrite endpoint=/bilibili.app.playerunite.v1.Player/PlayViewUnite frames=1 akamai_urls=1 decompressed_frames=0
[Bili Acc][media-request] rewrite media_host=upos.example.bilivideo.com method=GET range=true
```

网页端的 `media_urls` 会随响应内容变化；为 `0` 表示响应脚本执行了，但没有找到受支持的媒体 URL。原生 App 通常会出现 `[grpc-response]` 和/或 `[media-request] rewrite`；`akamai_urls=0` 表示响应内没有需要规范并包装到 `/proxy/` 的海外媒体地址，`decompressed_frames` 表示脚本处理的 gzip gRPC message 数量。请求方法不支持、媒体域名不匹配或响应与当前代理地址无关等正常放行情况不会打印日志；保留的 `skip reason=...` 仅用于说明参数无效、响应体为空、压缩/无效 protobuf 或 JSON 无法解析等需要排查的情况。日志只记录 API/媒体主机、无查询参数的 API 路径、状态和计数，不记录 server、Token、Cookie、完整媒体 URL 或查询参数。如果出现 `unsupported_content_type type=text/html`，说明 `/playurl-grpc/` 没有返回后端 gRPC 数据；应确认服务端和 Surge 模块均已更新到支持 binary HTTP tunnel 的版本，并检查 Caddy/CDN 是否将该路径改写成错误页。如果完全没有 `[Bili Acc]` 日志，说明请求脚本没有运行，或者请求被正常放行；需要排查时优先检查模块和 MITM 是否启用、Surge CA 是否已信任、实际请求是否命中模块声明的域名，以及是否有其他模块中更靠前的同类型脚本先匹配了该请求（Surge 对每个请求只运行首个匹配脚本）。

## 安全与运行行为

### Token 与日志

Token 位于代理 URL 路径中，因此以下位置可能看到完整请求路径：

- 宿主机 Caddy access log；
- CDN 或上游反向代理日志；
- 浏览器 Network 面板；
- 客户端代理软件记录。

应用使用标准库 `slog` 输出结构化事件，不记录完整 URL、查询参数、Token、Cookie、Authorization、原始 gRPC metadata 或 `grpc-message`。默认不逐条记录成功的媒体 `200/206` 请求，健康检查和 `OPTIONS` 也保持静默；playurl 与原生 gRPC 成功请求仍会记录。HTTP 失败、非零 gRPC status 和真实流错误会立即记录，相同的安全错误键在去重周期内只输出一次，并在下一次输出 `repeats_suppressed`。每个非空汇总周期输出一条 `traffic_summary`，包括请求数、成功/失败、流量、活动流、客户端取消、403 和上游响应头 p95。

客户端主动退出、拖动或切换清晰度导致的中断计为 `client_cancelled`，不写入常规逐请求日志，也不与真实 `stream_error` 混淆；诊断页成功率的分子和分母都会排除这些取消请求。playurl 日志中的 `quality_params=upgraded|failed|unchanged|not_attempted` 表示最高画质请求参数处理状态，不代表响应最终提供的实际画质。若启用 Caddy access log，应对 `/playurl/`、`/playurl-grpc/` 和 `/proxy/` 路径脱敏，或关闭不必要的访问日志。

### 上游连接

服务使用自定义 Go `http.Transport`：

- 强制 `tcp4`；
- 不读取 `HTTP_PROXY`、`HTTPS_PROXY` 或 `NO_PROXY`；
- 尝试使用 HTTP/2；
- 连接超时 10 秒；
- TLS 握手超时 10 秒；
- 响应头超时 20 秒；
- 空闲连接超时 90 秒；
- 每个上游最多保留 20 个空闲连接，最多建立 100 个连接。

playurl 整体请求超时为 30 秒，WBI key 获取阶段最多使用其中 3 秒。普通媒体流没有全局超时，但客户端断开时会取消对应上游请求。

### HTTP/3

HTTP/3 由宿主机 Caddy 在浏览器到 Caddy 的公网连接上提供。需要确保 UDP 443 可达。Caddy 到 Go 服务仍通过回环 HTTP 连接，因此启用 HTTP/3 不代表端到端 QUIC，也不保证提高 VPS 的持续媒体吞吐。

## 运行时诊断

### 实时诊断页

诊断页与代理共用主监听端口，通过公开服务地址访问：

```text
https://bili.example.com/diagnostics/
```

页面和 `/diagnostics/api/snapshot` 每 2 秒读取内存快照，展示活动媒体流、最近吞吐、1/5/15 分钟成功率与状态分布、CDN 主机错误率和 403、gRPC status、上游响应头 p50/p95、客户端取消及最近最多 100 条脱敏错误。指标使用固定的 900 个每秒时间桶保留最近 15 分钟：请求计数与状态不会因请求量超过事件环形缓冲区而低估，字节数按实际传输发生的秒计入窗口，延迟使用固定直方图，单桶主机数量受限并合并到 `other`。所有结构均有固定内存上限，不写入磁盘，进程重启后清零；页面不展示客户端 IP，也不会保存 URL、查询参数、Token 或认证头。

诊断响应设置 `no-store`、CSP、禁止 iframe 和 no-referrer 等安全头，并且不会进入代理请求指标或逐请求日志。该页面没有内置认证；需要限制访问时，可在宿主机 Caddy 中单独匹配 `/diagnostics/*` 配置 Basic Auth，或使用 Cloudflare Access。

### Flight Recorder

Compose 默认设置 `TRACE_DIR=/traces`，因此 Unix 容器会启用 Go Flight Recorder，在内存中保留最多约 8 MiB、至少最近 5 秒的运行时事件。

遇到偶发卡顿、锁等待或调度异常时，写出快照：

```bash
docker compose kill -s SIGUSR1 app
docker compose logs --tail=20 app
```

复制并打开最新 trace：

```bash
docker compose cp app:/traces/. ./traces/
TRACE_FILE=$(find ./traces -type f -name 'runtime-*.trace' | sort | tail -n 1)
go tool trace "$TRACE_FILE"
```

trace 保存在 Docker 命名卷中，不会随容器重建丢失。不需要该能力时，可以从 `compose.yaml` 中移除 `TRACE_DIR`、`volumes` 挂载和顶层 `traces` volume。

## 技术栈

- **Go 1.26** — HTTP 服务、流式代理、并发控制和运行时 Flight Recorder。
- **net/http** — 入站服务、上游请求、HTTP/2 和连接池。
- **Docker** — 多阶段静态二进制构建和非 root scratch 镜像。
- **Docker Compose** — 环境变量、回环端口、重启策略和 trace volume。
- **Caddy** — 宿主机 TLS 终止、反向代理和可选 HTTP/3。
- **Tampermonkey** — 网页端 playurl 拦截、Cookie 读取和媒体 URL 改写。
- **Surge** — iOS/macOS 请求改写、响应改写和 MITM 接入。

## 构建和测试

### 本地构建

```bash
go build ./cmd/bili-acc
```

### Go 测试

```bash
go test ./...
go test -race ./...
go test ./internal/proxy -run '^$' -bench . -benchmem
go vet ./...
```

### JavaScript 测试

```bash
node --test userscript/bili-cf-acc.test.cjs surge/bili-acc.test.cjs
node --check userscript/bili-cf-acc.user.js
node --check surge/bili-acc-request.js
node --check surge/bili-acc-response.js
```

### 部署验收

浏览器或 Surge 中至少检查：

- 网页 playurl 请求改写为 `https://你的域名/playurl/...`；
- 网页响应中的视频和音频 URL 改写为 `https://你的域名/proxy/...`；
- 原生 App 的媒体 CDN 请求直接改写为 `https://你的域名/proxy/...`；
- 点播视频和音频都能正常播放；
- 拖动进度时媒体响应为 `206 Partial Content`；
- 直播 playlist、分片和密钥请求都经过 `/proxy/`；
- `docker compose logs app` 中成功媒体请求不会逐条刷屏，失败会立即输出且重复错误会汇总；
- `https://你的域名/diagnostics/` 能显示滚动指标和脱敏错误；
- 日志和诊断快照中没有 Cookie、Authorization、Token、完整 URL 或查询参数。

## 许可证

本项目采用 [MIT License](LICENSE)。使用本项目时仍需遵守相关平台的服务条款。