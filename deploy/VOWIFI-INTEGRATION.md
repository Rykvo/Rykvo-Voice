# APN / VoWiFi

## APN
- 按 SIM 保存数据 APN，读取、编辑与应用分开。
- 保存不启用流量；应用仅写数据 CID 1，不改 IMS / SOS，不激活 PDP。
- 写入前校验卡身份、设备代次与占用；部分失败明确报告，不自动重试写入。
- 密码不回传浏览器，保留密码须显式标记。

## 工作进程
- `backend/internal/vocat` 提供 SIM/AKA、IKE/ESP、IMS 与生命周期核心；保留来源、许可证与修改说明。
- 私有 Unix socket 启动 VoWiFi 工作进程，仅该进程使用所需网络能力。
- 按模块保护设备和网络资源；停止后确认清理再释放，清理未确认时暂缓后续写入。
- 根据 SIM / 运营商资料匹配参数，不按模块编号或电话号码硬编码。
- 用户关闭开关、换卡或设备世代变化时取消旧连接；重连不覆盖用户选择。
- 注册、续期、网络恢复、号码同步、SMS/MMS 与通话分别验证。

## 资料
- [运营商兼容与验收](WIFI-COMPATIBILITY.md)
- [模块与 eSIM](../backend/MODULES.md)
- [SIP 电话](SIP-CALLS.md)
- [彩信](MMS.md)
