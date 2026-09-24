# 部署维护

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

## 彻底卸载

`sudo rykvo uninstall` 会要求输入 `DELETE rykvo_voice`。确认后删除应用程序、数据库和角色、账号、运行配置、所有应用备份、缓存、应用日志、服务及管理命令，不再保留恢复数据。操作不可撤回。

仍绑定 Cloudflare Tunnel 时，先在“云服务器”页面注销，脚本才继续卸载，避免遗留远端 DNS 或隧道。卸载不删除其他项目、共用的 PostgreSQL/Nginx 等系统软件包或系统审计日志。

更新前产生的备份仅保留到主动卸载之前；若需自行保留，请在执行卸载前手动移出应用备份目录。

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

已接通：登录、管理员改密、独立密码、功能显示、Cloudflare Tunnel、模块自动发现与只读状态、唯一标签。**短信、SIP、模块控制、任务队列和图片存储仍待实现**；不安装空转的 RabbitMQ / S3 服务，也不把环境安装当作业务已完成。

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

## 1.1.0 设备运行环境

新增 libqmi-utils、libpcsclite1、pcscd、libccid、polkitd 和应用专用 udev/polkit 规则。服务仍以 rykvo_voice 运行，不关闭其他设备管理程序，不调整模块射频。规则随更新备份/回退，完整卸载时删除本应用规则；共用系统组件保留。

模块能力与验证边界见 [模块读取](../backend/MODULES.md)。生产虚拟机是否透传 USB 需要单独检查，更新程序不会自动配置宿主机 USB 透传。
