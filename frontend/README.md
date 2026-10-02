# 前端

原生 HTML、CSS、JavaScript，简洁 Apple 风格，使用本地资源与系统字体。

## 入口
- 局域网：`http://主机IP/`；绑定域名：`https://域名/gly`。
- Go 按登录会话提供登录页或业务页；业务资源也需鉴权。
- 切换页面只更新必要节点，保留搜索、滚动位置及正在填写的内容。
- 凭据、设置和用户存储不作为缓存清理。

## 文件
| 文件 | 职责 |
| --- | --- |
| `index.html`、`app.js` | 页面与导航 |
| `login.*`、`auth.*` | 登录与会话 |
| `http.js`、`api.js` | 同源请求、取消与错误处理 |
| `shared.js`、`base.css`、`fields.css`、`forms.*` | 公共组件 |
| `modules.*`、`cellular.*` | 模块、eSIM 与蜂窝设置 |
| `messages.*`、`message-*.js` | 消息与会话 |
| `sip.*`、`sip-history.js`、`sip-server.js` | SIP 账号、记录与网络设置 |
| `host-settings.js`、`cleanup.js`、`contact-settings.js` | 主机名、清理与联系我们 |
| `software-update.js` | 上传更新与安装状态 |
| `assets/` | 已引用的本地图标 |
| `tests/` | 回归与隔离预览 |

接口见 [BACKEND.md](BACKEND.md)，业务 Tunnel 见 [TUNNEL.md](TUNNEL.md)。

## 检查
```bash
node --test frontend/tests/*.test.cjs
```
预览工具只用于隔离检查，不作为生产服务器。检查桌面、窄屏、深浅色与键盘操作。

## 素材
品牌图标由用户提供；其他图标与字体说明见 `THIRD_PARTY_LICENSE.txt`。区号参考 Google libphonenumber，仅用于显示，不表示号码有效或可达。本项目不是 Apple 官方产品。
