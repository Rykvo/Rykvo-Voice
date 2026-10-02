# Rykvo Voice

通信模块的 Web 管理平台，支持短信和彩信收发、SIP 电话、模块分配、主网络切换及 VLESS，通过 API 和 Webhook 与 Rykvo Text 对接。

正式发布线从 **1.1.0** 起；旧测试版请在备份后另行重新安装。

## 安装

支持 Ubuntu 24.04 / 26.04、Debian 13（amd64 / arm64），在主机上执行：

```bash
curl -fsSL https://raw.githubusercontent.com/Rykvo/Rykvo-Voice/main/deploy.sh | sudo bash -s -- install
```

按提示设置管理员密码，安装完成后访问 `http://主机IP/`。

## 卸载

如已绑定云连接，先在管理页面注销，再在主机上执行：

```bash
sudo rykvo uninstall
```

按提示输入 `DELETE rykvo_voice` 确认。卸载会永久删除本项目的程序、账号、数据库、配置和备份；其他项目及共用系统组件保留。

## 文档

[使用与维护](deploy/README.md) · [接口说明](frontend/BACKEND.md)
