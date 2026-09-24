# Rykvo Voice 后端

Go + PostgreSQL。实现登录、会话恢复、退出、管理员修改、独立密码、功能显示配置与 Cloudflare Tunnel，新增模块自动发现、只读状态与唯一标签，未实现短信、SIP 和模块控制接口。详见 [模块读取](MODULES.md)。

## 数据

- `administrators`：账号、独立随机盐、PBKDF2-HMAC-SHA256 哈希、迭代次数；不保存明文密码。
- `login_sessions`：随机会话标识的 SHA-256 哈希、用户归属、CSRF 令牌、12 小时绝对有效期。
- 初始化从标准输入读取默认密码，仅在管理员表为空时创建 `admin`，更新代码不重置密码。
- 浏览器仅持有 HttpOnly、SameSite=Strict Cookie，不使用 localStorage 保存密码或会话。
- 每个 IP 每分钟最多 5 次登录，同时最多处理 2 次密码校验；失败提示不区分账号不存在或密码错误。

密码派生使用 [Go 标准库 crypto/pbkdf2](https://pkg.go.dev/crypto/pbkdf2)，600,000 次迭代、16 字节随机盐、32 字节哈希。

## 独立密码与功能显示

- `visibility_security` 保存独立密码的盐、哈希、迭代次数及版本，不复用登录密码。
- `visibility_preferences` 保存账户级显示标志，关闭显示不会关闭业务服务。
- `visibility_attempts` 持久化失败计数：同账号 15 分钟最多 5 次失败；服务重启不清空。
- 连续 10 次图标点击、2 秒间隔重置在 Go 服务端判断；前端不含阈值和初始密码。
- 校验通过后，当前登录会话获得 5 分钟独立授权；窗口关闭撤销，改密撤销所有会话的独立授权。
- 改密需独立授权和当前独立密码，8–128 字节新密码。数据库事务与版本检查阻止旧授权并发写入。
- 首次初始化使用 `./rykvo-auth -init-visibility`，从标准输入提供密码；已有密码不覆盖。
- 配置读取需登录，写入和改密还需独立授权；所有写操作检查 Origin 与 CSRF。
- 浏览器中的监听逻辑仍可被分析。服务端授权是保护边界，不以混淆或隐藏 JavaScript 代替验证。局域网 HTTP 80 保留，Cloudflare Tunnel 域名通过 HTTPS 访问。

## 运行

Go 1.27+、PostgreSQL 16+。依赖固定在 `go.mod` / `go.sum`。

```sh
go test ./...
go vet ./...
go build -trimpath -o rykvo-auth .
export DATABASE_URL='postgres:///rykvo_voice?host=/var/run/postgresql&user=rykvo_voice'
# PUBLIC_ORIGIN 可选；本机网卡地址和已绑定域名自动校验。
# 首次部署：通过标准输入向 ./rykvo-auth -init 提供密码（不带换行）。
./rykvo-auth
```

数据库采用本机 Unix socket 的 peer 身份认证。系统用户和数据库角色均为 `rykvo_voice`，无超级用户权限。不开放数据库公网访问。

## 接口

| 方法   | 路径           | 请求 / 结果                                                   |
| ------ | -------------- | ------------------------------------------------------------- |
| POST   | `/api/session` | `{username,password}`；成功设置 Cookie 并返回会话             |
| GET    | `/api/session` | 有效 Cookie 返回会话；否则 401                                |
| DELETE | `/api/session` | 同源 Origin + `X-CSRF-Token`；删除当前数据库会话并过期 Cookie |

响应：`{data:{user:{id,username},csrfToken,expiresAt}}`。失败：`{error:{code}}`。不返回密码或密码哈希。

写操作仅接受本机网卡 HTTP 80、回环地址、可选 `PUBLIC_ORIGIN` 或已绑定域名的精确 HTTPS Origin；不放行任意域名或其他机器的局域网 IP。Nginx 代理 `/api/` 至 `127.0.0.1:8080` 并覆写 X-Real-IP。其他 API 未登录返回 401，已登录返回 503；没有假成功响应。

当前 HTTP 80 仅用于本地虚拟机。绑定域名通过 HTTPS 访问时使用 Secure Cookie，局域网入口不变。公开访问前通过管理员页面更换默认密码。

## 新安装器目标位置

- 服务：`rykvo-auth.service`（非 root，开机启动）
- 程序：`/opt/rykvo-voice/live/rykvo-auth`
- 源码：本仓库 `backend/`（生产包不包含源码）
- 数据库：`rykvo_voice`
- 前端：`/opt/rykvo-voice/live/web`

```sh
systemctl status rykvo-auth nginx postgresql
journalctl -u rykvo-auth -n 30
```

备份数据库时使用 `pg_dump`，不要把账号数据或数据库备份放进前端静态目录。源码和前端发布包均不含默认密码。

## 集成测试

在全新隔离数据库（名称以 `_test` 结尾）设置 `TEST_DATABASE_URL` 后运行 `go test ./...`。测试覆盖独立校验、授权过期、密码修改、撤销、数据库限流和显示配置写入，不使用生产数据库。

## 页面与账户

`WEB_ROOT=/opt/rykvo-voice/live/web`。Go 在局域网根地址、域名 `/gly` 按会话选择 `login.html` 或 `index.html`，业务资源均校验会话，不经过 Nginx 静态直出。登录公共资源使用显式白名单。响应禁止缓存，未登录业务资源返回 401。

`GET /api/administrator` 返回 `{data:{account}}`。`PATCH` 接收 `{account,currentPassword,newPassword,confirmPassword}`，验证当前登录密码，事务更新账号及密码哈希，撤销全部登录会话，返回 204。`administrator_attempts` 持久化 15 分钟 5 次限额。与功能显示密码完全分开。

升级执行 `rykvo-auth -migrate`，只更新表结构，不重置密码或配置。

## Cloudflare Tunnel

- `tunnel.go`：单绑定状态机、数据库持久化、恢复与鉴权接口。
- `cloudflare.go`：授权证书、区域校验、隧道和 DNS API；保守检查资源归属。
- `tunnel_cleanup.go`：有界退避重试、断点恢复及脱敏清理日志。
- `tunnel_process.go`：受限文件、授权子进程、连接器监督与健康检查。
- `tunnel_settings`：不含密钥的单例状态。凭据仅存 systemd StateDirectory 下，目录 0700、文件 0600。
- 依赖官方 cloudflared。当前验证版本 `2026.9.1`（Linux amd64），SHA-256 `03f1f25d1cc93b9ad6c60569d44060bc4f17ed97075760ed8cfca4b12dcd68cc`。
- 服务环境：`TUNNEL_BIN=/opt/rykvo-voice/live/cloudflared`、`TUNNEL_DIR=/var/lib/rykvo-voice/tunnel`。缺省不启用；配置后升级先执行 `-migrate`。
- 云服务器页面填写域名，点击连接后进入 Cloudflare 授权。没有实际域名授权时，不视为完成外网联调。
- `GET /api/tunnel`、`POST /api/tunnel/connect {domain}`、`POST /api/tunnel/disconnect {}`。协议见前端 `TUNNEL.md`。
- 连接接口按管理员限制每分钟 5 次。状态查询不计入登录限流。
- 连接器断线重连，服务重启恢复；注销失败不丢弃资源归属，不覆盖已有 DNS。
- 隧道只转发网页/API 到本机 Nginx 80，不代表 SIP/RTP 或短信通信已接通。

单元测试使用本地假 Cloudflare API，不创建真实资源；数据库测试使用全新 `_test` 数据库和假连接器，覆盖授权隔离、取消、临时故障、限流、连接释放、幂等删除及中断注销恢复。

Tunnel 源站固定为 `http://127.0.0.1:80`。Nginx 监听所有本机地址；登录校验随网卡 IP 变化，无需写入固定局域网 IP。进程先独占 API 监听端口，再恢复 Tunnel，避免重复后端进程同时启动连接器。

注销失败可查看 `journalctl -u rykvo-auth --grep=tunnel_cleanup`。接口保留 `errorCode=DISCONNECT_FAILED`，并附带 `cleanupStage` 和 `cleanupCode`；不把云端原始错误返回前端。进行中的注销重启自动继续，权限或归属错误保留为失败等待处理。

域名请求统一挂载在 `/gly`，包括 `/gly/api/*` 和受保护的静态资源；域名根路径返回 404，不跳转或泄露管理入口。本机 IP、回环地址仍使用 `/` 与 `/api/*`，不写死 IP 或域名。会话 Cookie 与数据库会话保留，路径不是权限校验的替代。
