# Bili CF Acc

固定出口 IP 的 B 站媒体代理。playurl API 与视频、音频、直播媒体均通过同一 VPS 出口，支持 Range/206 和 HLS URL 改写。

## 部署

要求：Linux VPS、Docker Compose、宿主机 Caddy，以及指向 VPS 的域名。

```bash
cp .env.example .env
# 编辑 DOMAIN 和 TOKEN
docker compose up -d --build
```

容器仅监听 `127.0.0.1:8080`。宿主机 Caddy：

```caddyfile
bili.example.com {
	reverse_proxy 127.0.0.1:8080 {
		flush_interval -1
	}
}
```

更新：

```bash
git pull
docker compose up -d --build
```

## 用户脚本

修改 `userscript/bili-cf-acc.user.js`：

```js
const SERVER = "https://bili.example.com";
const TOKEN = "与 .env 相同的 TOKEN";
```

安装到 Tampermonkey 并授权 `GM_cookie`。Cookie 仅随 playurl 请求转发，不会由应用写入日志或磁盘。

## Surge

Surge iOS 5.9+ / Mac 5.5+ 可安装 `https://raw.githubusercontent.com/JohnsonRan/Bili-Acc/main/surge/bili-acc.sgmodule`，填写 `server` 和 `token`，并启用 MITM、信任其 CA。模块仅 MITM B 站 playurl API，媒体响应仍由服务端流式转发。

## 配置

| 环境变量 | 默认值 | 用途 |
| --- | --- | --- |
| `TOKEN` | 必填 | 私有代理令牌 |
| `LISTEN_ADDR` | `:8080` | HTTP 监听地址 |
| `PUBLIC_URL` | 从请求推断 | HLS 改写使用的公开地址 |
| `ALLOWED_HOSTS` | B 站媒体域名 | 媒体域名后缀白名单 |

服务默认强制上游使用 IPv4，并忽略 `HTTP_PROXY`、`HTTPS_PROXY`，确保 playurl 与媒体请求使用同一出口。

## 检查

```bash
go test ./...
go vet ./...
node --test userscript/bili-cf-acc.test.cjs surge/bili-acc.test.cjs
node --check userscript/bili-cf-acc.user.js
node --check surge/bili-acc-request.js
node --check surge/bili-acc-response.js
```

浏览器 Network 中应看到 `/playurl/`、`/proxy/` 请求；拖动播放进度时媒体响应应为 206。

## 备注

- playurl 请求会提升到账号可用的最高画质并重签 WBI；响应元数据和清晰度列表保持上游原样。
- 普通媒体使用固定缓冲区流式转发；HLS playlist 最大 4 MiB。
- 不缓存媒体和 Range 响应。
- HTTP/3 由宿主机 Caddy 提供，公网需放行 UDP 443。
- Token 位于代理路径中，宿主机反向代理日志需自行脱敏。
