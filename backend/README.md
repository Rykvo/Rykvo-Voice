# 后端

Go API、PostgreSQL 持久化、模块控制、SMS/MMS、SIP 与业务 Tunnel。

## 运行
依赖版本固定在 `go.mod`、`go.sum` 与 `deploy/runtime.json`。

```bash
cd backend
go test ./...
go vet ./...
go build -trimpath -o rykvo-auth .
```

生产由安装器配置低权限用户 `rykvo_voice`、数据库 Unix socket 和 systemd。API 监听 `127.0.0.1:8080`，经本机 Nginx 访问。

## 数据与权限
- 管理员密码、会话和设置持久化至数据库；密码使用随机盐派生哈希。
- 管理写入验证会话、Origin 和 CSRF；独立显示密码的授权和限流由服务端管理。
- 开发者 API 使用 API Key，按模块与任务校验权限、幂等和限流。
- 数据库迁移兼容已有数据；升级不重置密码、会话或设置。
- 业务 Tunnel 凭据保存在私有运行目录，不放入数据库响应、页面或日志。
- 更新只接受经过校验的本机上传包，通过私有 socket 执行固定安装任务。

## 检查
```bash
systemctl status rykvo-auth
journalctl -u rykvo-auth -n 30
sudo /opt/rykvo-voice/live/rykvo-auth -maintenance health
```

数据库测试使用全新 `_test` 数据库，设置 `TEST_DATABASE_URL`；运行时集成测试使用独立的 `TEST_RUNTIME_DATABASE_URL`。不指向生产库。

## 文档
- [接口](../frontend/BACKEND.md)
- [模块与 eSIM](MODULES.md)
- [SIP 电话](../deploy/SIP-CALLS.md)
- [部署维护](../deploy/README.md)
- [测试](../deploy/TESTING.md)
