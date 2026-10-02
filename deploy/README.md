# 部署维护

## 运行
- Linux amd64 / arm64，安装器配置 Nginx、PostgreSQL、Python、设备依赖和 systemd 服务。
- 首次安装自动配置升级密钥，交互设置管理员和功能显示密码；更新保留账号、会话和配置。
- 局域网访问 `http://主机IP/`，绑定域名访问 `https://域名/gly`。
- API 只监听 `127.0.0.1:8080`，数据库使用本机 Unix socket。

## 网络
[网络配置](NETWORK-CONFIG.md)：通用 → 网络配置，管理主网络、模块分配及每网络独立 VLESS，重启保留配置。

## 目录
| 路径 | 用途 |
| --- | --- |
| `/opt/rykvo-voice/live` | 当前程序与页面 |
| `/var/lib/rykvo-voice` | 运行数据与业务 Tunnel 凭据 |
| `/var/lib/rykvo-update` | 更新暂存、状态与安装日志 |
| `/var/lib/rykvo-network` | 主网络与 VLESS 私有配置 |
| `/var/backups/rykvo-voice` | 更新回滚备份 |

## 更新与检查

**支持跨版本直接升级，不需要逐级安装。** 本正式发布线从 1.1.0 起可直接升级到后续版本，中间版本无需安装。旧测试发布线另行重新安装。

- 每个版本提供完整 RVU 更新包，包含程序、页面、依赖与累计数据迁移；不依赖中间版本的残留文件。
- 在「通用 → 软件更新」上传目标版本的签名加密 RVU，支持断线续传；校验通过并确认后，先排空业务、备份，再安装和检查。
- 保留账号、会话、配置、短信彩信、模块分配和 SIP 数据；检查失败恢复原程序，不覆盖业务数据库。
- 每次发版使用 1.1.0 原始升级器验证新包的 amd64 / arm64 兼容性，沿用签名信任链与升级密钥。仅允许向前升级或同版本修复，不支持降级。

准备包在执行结束后清理；保留最后状态与诊断日志。

```bash
systemctl status rykvo-auth rykvo-network rykvo-update.socket nginx postgresql
journalctl -u rykvo-auth -n 30
journalctl -u rykvo-update -n 30
sudo /opt/rykvo-voice/live/rykvo-auth -maintenance health
```

更新任务运行时不要关闭主机或删除准备目录。排空超时会停止更新，不强制中断通话。

## 数据清理
临时附件默认保留 2 小时，可设置 1–8760 小时；业务历史默认 3 天，可修改，0 保留全部历史。未完成任务、有效附件和必要去重信息受保护。

## 卸载
`sudo rykvo uninstall` 需要输入确认文字，会删除本应用程序、数据库、配置、日志和备份。先在页面注销仍绑定的业务 Tunnel；共用系统软件包及其他项目保留。

## 维护文档
- [打包发布](UPDATE-POLICY.md)
- [测试](TESTING.md)
- [接口](../frontend/BACKEND.md)
- [模块与 eSIM](../backend/MODULES.md)
- [SIP 电话](SIP-CALLS.md)
- [业务 Tunnel](../frontend/TUNNEL.md)
