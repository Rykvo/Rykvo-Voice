# Rykvo Voice

原生静态前端 + Go API + PostgreSQL。支持本机 IP 根地址和域名 `/gly`，Cloudflare Tunnel 在页面内配置。

## 私有仓库部署

目前只使用本私有仓库：完整源码、部署工具和私有 Releases 编译包均需要仓库读取权限。未创建公开仓库。以后需要给别人免仓库权限安装时，再单独安排发布渠道；源码仓库仍保持私有。

SSH 登录服务器，在 SSH 终端登录 GitHub，然后安装：

```bash
sudo apt-get update && sudo apt-get install -y gh git
# 选择 GitHub.com / HTTPS，并完成终端提示的登录授权
gh auth login
gh repo clone Rykvo/Rykvo-Voice
cd Rykvo-Voice
sudo bash install.sh
```

菜单：**安装 / 更新 / 卸载 / 状态**。安装脚本读取当前用户的 GitHub CLI 登录凭据（支持 sudo 原用户），下载私有编译包，不额外保存令牌。没有 CLI 登录时，会提示隐藏输入仅具本仓库 Contents: Read 权限的令牌。

已有编译包也可解压后直接运行 `sudo bash install.sh`，无需再次访问仓库。

Windows 本机也可运行 `deploy.ps1` 或双击 `Deploy.cmd`。需要 OpenSSH 和已登录的 GitHub CLI；脚本下载私有编译包、校验后通过 SSH 上传，不把 GitHub 令牌传到服务器。支持 `-User`、`-Port`，默认 root/22，不保存 SSH 密码，不关闭主机指纹校验。

```powershell
.\deploy.ps1 -Server HOST -Action install
.\deploy.ps1 -Server HOST -Action update
.\deploy.ps1 -Server HOST -Action uninstall
```

安装后：

```bash
sudo rykvo
sudo rykvo update
sudo rykvo uninstall
sudo rykvo status
```

更新使用已有 GitHub CLI 登录或临时只读令牌；已有账号、数据库和配置不重置。SSH 密钥可减少本机工具的重复认证。

支持 **Ubuntu 24.04 / 26.04、Debian 13，amd64 / arm64，systemd**。建议独立服务器，至少 2 GB 内存、4 GB 空闲磁盘。80 和 8080 不应被其他业务占用。

## 自动完成

- 安装 Nginx、PostgreSQL、Python 和系统依赖，启用开机启动。
- 校验安装包 SHA-256；直接使用编译程序，无需在用户服务器安装 Go、Node.js 或构建源码。cloudflared 随安装包提供，构建时校验官方 SHA-256。
- 检查端口和站点冲突，不覆盖其他业务站点，不自动调整防火墙。
- 创建低权限系统账户和数据库角色；数据库使用本机 Unix socket，API 只监听 `127.0.0.1:8080`。
- 数据库迁移、静态资源白名单发布和健康检查；源码编译和测试在维护环境完成。
- 更新前停止应用并备份数据库、Tunnel 凭据及配置；不重置密码、设置和会话。
- 发布使用原子链接切换；失败恢复上一套程序和 Nginx 配置。数据库迁移采用兼容性新增，不自动用旧备份覆盖更新后的数据。
- 安装服务、命令行管理入口；前端运行时不依赖 Node.js 或 Python。

**首次安装**：账号 `admin`，交互设置管理员密码和功能显示独立密码，回车会随机生成。密码不写入仓库、参数或日志；数据库只保存带随机盐的派生哈希。已有安装升级不询问或重置密码。

**访问**：局域网 `http://服务器IP/`；页面绑定的域名 `https://域名/gly`。域名根路径返回 404。云连接指向本机 `127.0.0.1:80`，不写死服务器 IP 或域名。局域网 HTTP 不提供传输加密；对外使用 Tunnel 的 HTTPS。防火墙如已启用，按实际需要仅向局域网放行 80；8080 与数据库无需对外开放。

## 卸载与恢复

普通卸载停止应用和连接器，删除本部署器管理的程序发布、服务及 Nginx 站点；保留数据库、凭据、备份和小型管理入口，便于重装。系统软件包和其他服务不删除。**Cloudflare 上的 DNS 与 Tunnel 不会因本地卸载自动删除**；需要清除远端资源时先在网页“云服务器”注销。

```bash
sudo rykvo uninstall --purge
```

彻底卸载要求先注销云连接，并手动输入 `DELETE rykvo_voice`，再删除应用数据库、角色、运行状态和缓存；不会删除其他数据库。自动备份仍保留在 `/var/backups/rykvo-voice/`，包含敏感数据，仅 root 可读，应按自己的保留策略离线归档或删除。

备份目录包含 `database.dump`、`state.tar.gz`、旧服务和 Nginx 配置。**不要公开上传这些备份**。需要数据恢复时先停应用，核对备份和数据库后使用 `pg_restore`；避免在仍有写入的系统直接覆盖恢复。保留的数据库和状态会在重装时继续使用。

## 维护者：构建与私有发布

在本私有仓库维护代码，通过检查后增加 `VERSION`（例如 `1.0.1`）。Linux 构建环境使用 `deploy/runtime.json` 中的 Go 版本：

```bash
python3 deploy/build.py --go /path/to/go --output dist
```

产物为 amd64 / arm64 两个安装包与 `release.json`。安装包只从明确白名单打包，不包含 `backend/`、测试和生产数据。所有发布资产仍放在本私有仓库，未开放匿名下载。

```bash
gh release create v1.0.1 dist/rykvo-voice-linux-amd64.tar.gz dist/rykvo-voice-linux-arm64.tar.gz dist/release.json --repo Rykvo/Rykvo-Voice --title v1.0.1 --notes "更新说明"
```

发布命令只在维护者已授权的本机/CI 执行；令牌不打包到产物中。先运行测试并检查安装包文件清单，再发布。不要覆盖已有版本资产；更新使用新版本号。Go 与 cloudflared 的固定版本和校验和在 `deploy/runtime.json` 中统一维护。

## 代码与接口

| 目录 | 内容 |
| --- | --- |
| `frontend/` | 页面与本地静态资源；不含生产账号和配置 |
| `backend/` | Go API、数据库结构与测试 |
| `deploy/` | 服务配置、运行时版本、校验和与发布工具 |
| `tests/` | 部署工具测试 |

已接通：登录、管理员改密、独立密码、功能显示、Cloudflare Tunnel。**短信、SIP、模块通信、任务队列和图片存储仍待实现**；不安装空转的 RabbitMQ / S3 服务，也不把环境安装当作业务已完成。

- [后端协议](frontend/BACKEND.md)
- [云连接协议](frontend/TUNNEL.md)
- [后端维护](backend/README.md)

```bash
node --test frontend/tests/*.test.cjs
python3 -m unittest discover -s tests
bash -n install.sh
cd backend && go test ./... && go vet ./...
```

数据库集成测试需全新名称以 `_test` 结尾的数据库，设置 `TEST_DATABASE_URL`；不要指向生产数据库。CI 同时运行前端、Go、部署工具和数据库测试。

依赖来源：[Go](https://go.dev/dl/)、[cloudflared](https://github.com/cloudflare/cloudflared/releases)、[PostgreSQL](https://www.postgresql.org/download/linux/ubuntu/)。素材说明见 `frontend/THIRD_PARTY_LICENSE.txt` 和前端文档。
