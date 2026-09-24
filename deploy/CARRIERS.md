# 运营商适配

统一流程：读取 SIM 归属网络及可用的 SPN/GID1/GID2 → 最具体的配置匹配 → IKE/EAP/IPsec → IMS 注册。

- 内置配置独立提取自 `dwilliamsuk/ios-carrier-bundles` 的运营商 bundle，仅保存互通参数，不包含参考项目实现、SIM 身份或凭据。生成输入 SHA-256 保存在 JSON 中。
- 当前导入 641 条规则。规则存在不代表网络已验证、套餐已开通或通话/短信业务已经实现。
- 未匹配的卡使用标准默认流程；不按模块编号、国别猜测运营商品牌。GID/SPN 等约束缺失时不匹配该专用规则。
- TCP 使用隔离的 gVisor 用户态协议栈，隧道地址只存在于进程内，不改主机网络、路由或防火墙。IPv6 链路 MTU 为 1280。所有流量仍验证协商的流量选择器及 ESP 完整性/重放窗口。
- 支持 TCP/UDP、多 P-CSCF、客户端/服务端 IMS 安全关联。只在首次没有匹配 SIP 响应时切换传输；鉴权失败、证书失败或明确拒绝不降级重试。
- 续期使用本机会话 Contact 的实际有效期。无 qop 的 AKA 应答不重复使用；保留 IPsec 安全关联。遇到 SIP 503 时遵守 Retry-After，避免连续请求。
- 连接状态不代表通话和 IMS 短信可用；这两项能力仍保持关闭。

## 更新数据

维护机运行 `python deploy/import-carriers.py BUNDLES.tar.gz backend/internal/hardware/wifi_carriers.json`。导入器读取归档中的 plist，不解压或执行 bundle。不导入关闭证书验证等设置。数据更新必须审查、测试并重新发布安装包。

## 外部配置

服务启动时读取 `/etc/rykvo-voice/carriers.d/*.json`（可用 `RYKVO_CARRIER_DIR` 指定其他目录）。不会自动下载配置。无效文件使相关 Wi-Fi 尝试明确失败；不会忽略损坏配置继续猜测。

```json
{
  "version": 1,
  "profiles": [{
    "id": "example-fixture",
    "match": [{"plmn": "00101", "gid1": "AB"}],
    "transport": "tcp",
    "contact": "gsma",
    "agent": "Rykvo-Voice",
    "country": "GB",
    "tags": ["+g.3gpp.smsip"]
  }]
}
```

同一匹配项的条件全部满足才生效；多个匹配项为或关系。更具体的规则优先，同分时后加载的外部规则优先。修改后在维护窗口重启应用加载，不重启服务器或模块。配置不包含密码、AKA 密钥、实际订户身份。

## 诊断

日志只记录模块编号、配置 ID、传输协议和受限错误码；不记录原始 SIP 鉴权报文或 SIM 密钥。API 的 `wifi.carrierProfile` / `wifi.transport` 用于区分匹配结果和真实注册状态。IMS 响应超时、TCP 连接失败、加密注册失败分别显示。

维护探针的 `HoldSeconds` 上限为 3600。小于 300 秒时在第 30 秒测试提前续期；至少 300 秒时遵守网络授予的自然续期时间，首次续期成功后退出并验证注销、射频恢复。探针超时而未出现 `renewed` 不算续期验证通过。生产运行不强制提前续期。
