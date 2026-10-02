# Wi-Fi Calling 兼容与验收

## Normative baseline

- GSMA IR.51 (UE/network IMS profile): https://www.gsma.com/newsroom/gsma_resources/ir-51-ims-over-wi-fi-v/
- 3GPP TS 24.302 (non-3GPP EPC access): https://portal.3gpp.org/desktopmodules/Specifications/SpecificationDetails.aspx?specificationId=1073
- 3GPP TS 24.229 (IMS registration and SIP): https://www.etsi.org/deliver/etsi_ts/124200_124299/124229/16.15.00_60/ts_124229v161500p.pdf
- RFC 3261 sections 10.2.8, 20.23 (423 / Min-Expires): https://www.rfc-editor.org/rfc/rfc3261.html#section-10.2.8
- AOSP carrier selection: https://source.android.com/docs/core/connect/carrier
- AOSP entitlement: https://source.android.com/docs/core/connect/ims-service-entitlement

These documents define shared behavior, not a universal operator activation file.
A catalogue match is not proof of registration, SMS, voice/media or MMS support.

## 维护规则

- 运营商匹配使用 home PLMN 与可用 GID/SPN/ICCID，不使用模块编号或号码前缀。
- ePDG 解析、IKE/IPsec 鉴权、IMS REGISTER 及续期独立检查；目录匹配不是连接成功证明。
- 423 / Min-Expires 只进行有界增大重试；注销仍使用 Expires: 0。
- ISIM 身份按卡原子读取、校验并在清理后复核，不混用新旧卡的身份。
- 号码优先使用已读取的 SIM 或经过验证的 IMS 号码，不从 IMSI/ICCID 推测号码。
- 网络恢复采用单一协调器和有界退避；用户关闭、鉴权拒绝和限流不进行无限重试。
- 各运营商的开户条件、entitlement、位置策略与协议差异分别核验，不用目录或标准替代实卡测试。

## 每个运营商分别验收
注册及自然续期、重启与网络中断恢复、手动关闭、短信双向、彩信双向、呼入呼出及双向音频、挂机资源释放。记录设备、固件和条件；短时测试不代表长期稳定或并发容量。
