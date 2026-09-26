# SMS / MMS

## 已接入
- 蜂窝短信：AT PDU 发送；逐个读取 SM/ME 存储并恢复原读取存储；不删除模块存储。
- Wi-Fi 短信：复用现有 IMS 会话，收件先入库再确认，发送与回执分别持久化。
- 请求 UUID 和内容摘要防重复；重启时将发送中任务标记为结果未知，不自动重发。
- 消息持久化、增量游标、附件鉴权、会话按模块/SIM/对端隔离；旧本地历史保留。
- APN/MMSC：固定版本 AOSP APN 和 Carrier ID 库，按 SIM 归属网络及 MVNO 条件匹配，不按漫游访问网络匹配。未知或冲突配置不猜测。
- MMS：有界 MM1 编解码、WAP Push、附件校验、确认响应检查及收件下载流程。

## MMS 运营商专用承载
蜂窝使用收件 SIM 的 EC20 MMS PDP；Wi-Fi 使用同一 SIM 的独立 MMS APN/ePDG 承载。DNS/TCP 绑定承载，不使用主机普通网络、不切换主机默认路由。Wi-Fi 与蜂窝配置分别匹配，详细流程见 MMS.md。
2026-09-26 电信蜂窝入站图片已实卡下载并入库；Wi-Fi SMS 正常不代表 MMS 承载已就绪。匹配到 APN/MMSC 不代表所有运营商均支持，Wi-Fi 彩信需单独验收。

## 行为边界
- `accepted` 仅代表网络接受；SMS 有可靠回执才标记 `delivered`，不表示已读。
- 确定尚未提交的网络等待可重试；写出 HTTP 请求后的断线/重定向保持结果未知。
- 保持现有 Wi-Fi/漫游互锁，不为 MMS 擅自关闭 Wi-Fi 通话或开启蜂窝射频。
- 暂不对服务器消息执行原本仅针对本地历史的自动清理；删除消息为软删除。
- 通话媒体、已读同步、大型附件、视频/音频彩信、多附件和运营商全覆盖均不在已验证范围。

## 测试
Go 单元测试、隔离 PostgreSQL API/幂等/收件测试、前端自动化及深浅色/窄屏 UI 验证。
实卡验收使用独立 UUID：未知结果不重复发送，不因页面刷新重新提交。
协议依据：[OMA MMS Encapsulation](https://openmobilealliance.org/release/MMS/V1_3-20050927-C/OMA-TS-MMS-ENC-V1_3-20050927-C.pdf)。
配置来源与哈希见 `backend/internal/carrierconfig/SOURCE.json`。
