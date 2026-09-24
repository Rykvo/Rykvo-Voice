# Cloudflare Tunnel

云服务器页填写域名 → 连接 → 授权。授权窗口选择该域名所属的 Cloudflare DNS 区域。域名由用户填写，不写死；不使用临时随机域名。

Go 后端、PostgreSQL 状态表及 cloudflared 已接入。真实域名的授权和外网访问需完成后再验收。当前页面只显示服务端状态，不模拟连接成功。

域名管理入口为 `https://域名/gly`，根路径返回 404；局域网仍直接使用 `http://本机IP/`。所有域名资源与接口使用 `/gly/` 前缀，登录、退出、刷新保持此入口。Tunnel 源站仍为本机 80 端口，无需改 DNS 或重新授权。

## 接口

以下为局域网路径；域名访问时加 `/gly` 前缀。所有接口须登录；写入还检查 Origin、CSRF 与 JSON 正文。

| 方法 | 路径                     | 正文                             |
| ---- | ------------------------ | -------------------------------- |
| GET  | `/api/tunnel`            | 无                               |
| POST | `/api/tunnel/connect`    | `{"domain":"panel.example.com"}` |
| POST | `/api/tunnel/disconnect` | `{}`                             |

响应：

```json
{ "data": { "status": "connecting", "domain": "panel.example.com" } }
```

- `disconnected`：未连接，可填写域名。
- `authorizing`：等待 Cloudflare 授权；返回本次 cloudflared 生成的 `authorizationUrl`，仅发起者可见，有效期最多 5 分钟。
- `connecting`：创建资源或连接中，可取消。
- `connected`：DNS 路由已配置，连接器 `/ready` 和本机应用健康检查通过；外网 DNS、证书传播仍需实际访问确认。
- `disconnecting`：停止连接并清理本应用资源。
- `failed`：`errorCode` 表示原因；可重试或取消。`DISCONNECT_FAILED` 只重试注销；`cleanupStage`、`cleanupCode` 返回最后失败步骤和脱敏原因。

进行中每 2.5 秒查询，已连接每 15 秒查询。隐藏或离开页面暂停轮询；取消浏览器请求不会停止后台服务。状态查询遇到短暂网络错误会退避重试，连续 5 次失败后停止，不重复提交注销。重启恢复已授权连接或继续进行中的注销；已失败的清理保留状态供手动重试。

相同域名重复连接复用同一资源；更换域名前必须先注销。接口为单绑定设计，不启用多域名并行管理。

## 配置与保护

- `tunnel-api` 已启用。未部署后端的普通静态文件服务器不具备授权或访问隔离。
- `TUNNEL_BIN=/usr/local/bin/cloudflared`，`TUNNEL_DIR=/var/lib/rykvo-voice/tunnel`；由 `rykvo-auth.service` 以非 root 身份管理子进程。
- 固定源站 `http://127.0.0.1:80`，不接受浏览器传入源站地址、命令或配置文件路径。监测端口仅监听 `127.0.0.1:20241`。
- PostgreSQL `tunnel_settings` 保存域名、资源 ID、归属和恢复状态。证书、隧道密钥保存在服务端目录（0700），文件权限 0600；不放入前端、数据库、浏览器存储或日志。
- 校验授权区域和域名边界。已有 DNS 记录一律提示冲突；仅复用本应用随机资源名标记的记录，不覆盖其他记录。
- 注销先正常停止连接器，再校验资源归属、删除对应 DNS、清理残留连接并删除隧道，最后清除本机凭据。只处理本应用资源，不使用级联删除。临时网络故障、限流及连接释放延迟每步最多尝试 5 次，退避 2/4/8/16 秒并尊重 Retry-After；总清理时限 2 分钟。权限或归属错误立即停止，保留凭据及进度。删除证书不等同于撤销账户侧 API Token；完整撤销在 Cloudflare「个人资料 → API Tokens」完成。
- 诊断写入服务日志 `tunnel_cleanup`：步骤、尝试次数、内部错误码、HTTP 状态和 Cloudflare 数字错误码；不记录 Token、授权地址、请求头或原始响应。
- 已绑定域名仅放行精确 HTTPS Origin，并设置 Secure Cookie；局域网原入口仍保留。本机网卡 IP 自动识别，Nginx 不绑定固定局域网 IP。Nginx 只信任本机 cloudflared 传入的客户端 IP，不信任局域网客户端伪造的头。
- 域名访问时注销会切断当前入口，请使用局域网入口继续操作。公开访问前更换默认登录密码。
- 此隧道接入的是网页和 HTTP API，不自动承载 SIP 注册、RTP 音频或短信模块连接。

官方参考：[本地管理隧道](https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/create-local-tunnel/)、[凭据权限](https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/tunnel-permissions/)。
