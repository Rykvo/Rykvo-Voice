# 后端对接

## 入口与权限

后端位于 `backend/`，使用 Go 与 PostgreSQL。模块、eSIM、SMS/MMS、SIP、异常通知、开发者接口和清理均由服务端处理；页面只展示实际结果，不用本地定时器假装成功。

- 网页局域网入口 `/`，域名入口 `/gly`；`http.js` 自动选择 `/api` 或 `/gly/api`。
- 外部开发者使用已连接主机域名的 HTTPS `/api/v1`，不使用 SIP 服务器域名。
- 网页接口验证 HttpOnly 会话；写入另验 Origin 与 CSRF。开发者接口独立验证 `X-API-Key`。
- 凭据不放 URL、localStorage、错误响应或日志。密钥读取仅返回“已设置”。
- 页面 HTML 和业务 JSON 不缓存；静态 JS/CSS/图标使用 `private, no-cache`，每次复用前仍经过会话检查。未登录业务资源返回 401，不因条件请求返回 304 而跳过登录。
- `index.html` 启用已接通的业务范围。离线预览不发请求；功能显示开关不停止后台业务，也不代替服务端权限。

## 网页接口

下表以局域网 `/api` 为前缀。客户端调用参数为 `{params,query,body,signal}`。请求超时为 15 秒，切页取消读取，不自动重试写入。业务幂等以正文 `requestId` 为准，不依赖浏览器请求头自动生成的键。

| 功能 | 接口 |
| --- | --- |
| 登录/会话/退出 | `POST/GET/DELETE /session` |
| 管理员 | `GET/PATCH /administrator` |
| 模块列表、详情、名称 | `GET /modules`、`GET/PATCH /modules/:moduleId` |
| 模块/主机重启 | `POST /modules/:moduleId/restart`、`/modules/restart`、`/modules/host-restart` |
| eSIM 下载、运营商通知 | `POST /modules/:moduleId/esim`、`/modules/:moduleId/esim/notifications` |
| 线路、Wi-Fi 通话、漫游、eSIM 切换 | `PATCH /modules/:moduleId/lines/:lineId` |
| 删除停用的 eSIM | `DELETE /modules/:moduleId/lines/:lineId` |
| 数据 APN | `GET /modules/:moduleId/lines/:lineId/apns`；单项 `PUT/DELETE /modules/:moduleId/lines/:lineId/apns/:apnId`、`POST /modules/:moduleId/lines/:lineId/apns/:apnId/apply` |
| 信息 | `GET/POST /messages`、`GET/DELETE /messages/:messageId`、`GET /messages/:messageId/image` |
| 会话摘要/整段删除 | `GET/DELETE /messages/threads` |
| 聊天分页/内容搜索 | `GET /messages/window`、`GET /messages/search` |
| 备注 | `PUT /messages/contacts` |
| SIP 账号 | `GET/POST /sip/accounts`、`GET/PATCH/DELETE /sip/accounts/:accountId` |
| 通话记录与统计 | `GET /call-records`、`GET /call-records/stats` |
| 开发者与机器人配置 | `GET/PATCH /settings/developer` |
| Webhook 测试/记录/重试 | `POST /settings/developer/webhook/test`、`GET /settings/developer/webhook`、`POST /settings/developer/webhook/replay` |
| 异常阈值 | `GET/PUT /settings/alerts` |
| 登录页联系链接 | `GET/PUT /settings/contact`；未登录仅 `GET /public/contact` 返回链接 |
| 主机名、自动清理 | `GET/PUT /settings/hostname`、`GET/PUT /settings/retention` |
| 主机域名连接 | `GET /tunnel`、`POST /tunnel/connect`、`POST /tunnel/disconnect` |
| SIP 服务器 | `GET /settings/sip-server`、`POST /settings/sip-server/connect`、`POST /settings/sip-server/logout` |
| 运行版本、后台健康 | `GET /version`、`GET /health` |

通用附件上传、网页安装更新、任务查询和 SSE 占位客户端已删除。实际消息附件随发送正文上传；外部开发者使用下面的 `/api/v1/attachments`。更新通过正式安装包执行，版本窗口显示运行二进制的真实版本。

### 功能显示与保留能力

- `POST /ui/activation` 在服务端累计入口手势，不代替认证。
- `GET/PUT /settings/visibility` 读写已知显示开关；写入需独立密码授权。
- `POST /settings/visibility/unlock` 校验独立密码，`GET/DELETE /settings/visibility/access` 检查/撤销当前授权。
- `PUT /settings/visibility/password` 需重新核对独立密码，成功后撤销全部独立授权，不更改登录密码。
- `POST /modules/:moduleId/lines/:lineId/emergency-address/session` 创建当前 SIM 的运营商验证会话，202 返回 `id/state/expiresAt`；不修改 Wi-Fi 开关、不提交地址。
- `GET /modules/:moduleId/lines/:lineId/emergency-address/session/:sessionId` 查询 `pending/ready/failed`；ready 返回经白名单校验的运营商 `page.url/page.token`。前端仅以表单 POST 将 token 交给该运营商内嵌页，不存储、不放进 URL。
- `DELETE /modules/:moduleId/lines/:lineId/emergency-address/session/:sessionId` 取消并清除内存会话；与创建操作一样要求登录和 CSRF。会话绑定登录、模块、SIM、换卡代数及设备代数，5 分钟过期；打开期间继续核对绑定。
- 页面入口只对已验证的运营商配置显示，未知 MVNO 不继承漫游网络入口。首批实测为 310/280 + GID1 20FF；其他运营商须分别接入真实鉴权协议和验收，不使用猜测的公网表单。

### 消息与游标

- `POST /messages`：`{requestId,moduleId,lineId,to,text,image?}`；`image` 为 PNG/JPEG/GIF data URL，最大 1 MiB。202 表示入队，重复同正文返回原记录，不重复发送。
- `GET /messages?after=0` 返回 `items,cursor,snapshot,more,contacts,contactCursor`。`more=true` 时沿用 `snapshot` 和返回的 `cursor`；完成后去掉 snapshot，继续拉新变化。备注单独携带 `contactAfter`。
- 游标超过已清理边界返回 410 `CURSOR_EXPIRED`，清空当前镜像后从 0 重建；不擅自删除开发者自己归档的数据。
- 网页会话列表改用 `GET /messages/threads`，游标字段与消息同步相同，每个模块/SIM/原始号码只传最新摘要；不在启动时下载全部聊天历史。数据库事务内同步维护摘要，旧消息状态变化不会替换最新预览。
- `GET /messages/window?moduleId=…&lineId=…&numbers=[…]` 每页 80 条。`direction=earlier/newer/current` 携带 `anchor` 消息 ID，以数据库时间与 ID 做键集分页；不靠不精确的浏览器毫秒时间截断。同时间戳不漏不重复，正在读旧页时保持位置。
- 浏览器只保留当前会话的一页聊天正文；会话列表默认显示 80 项，可加载更多。`GET /messages/search?text=…` 在服务器搜索未加载正文，结果超过 200 个会话时提示缩小范围。图片延迟加载不把正在阅读历史的页面拉到底部。
- `DELETE /messages/threads` 正文为 `{moduleId,lineId,numbers}` 或 `{all:true}`。每次最多软删除 256 条，沿用返回的 snapshot 直到 more=false；删除包括未加载历史，但不删除开始操作之后新到的消息。
- 页面“已送达”按产品约定包含运营商明确接受。底层仍区别 `accepted` 与 `delivered`，未知结果不当作成功，也不自动重发。
- 删除是服务端软删除；排队发送会取消，已提交的发送不能通过删除撤回。未完成工作、去重留痕和待推送事件按清理规则保护。
- 号码等价合并只依据同一 SIM 已出现且无歧义的国际号码；不凭本地号码猜国家，不跨 SIM 合并。备注用 lineId 与号码定位。

### 模块与 eSIM

模块 ID、lineId 是稳定标识，不使用页面数组下标。号码未知时返回空号码，不拿 ICCID 冒充电话号码。

eSIM 写入必须带 requestId 与 EID，下载提供 activation，必要时提供 confirmation/imei。激活码不落日志、数据库或浏览器存储。202 返回任务，不代表完成；读取 `Module.job` 的状态、阶段和核验结果。同一请求不重复写卡；进程重启后不自动重放结果不确定的写入。

蜂窝串口、Wi-Fi SIM 会话、SIP 音频和配置写入按模块协调；慢模块不持有全局状态锁。eSIM、Wi-Fi 切换和重启不会抢占本模块已登记的收发/通话工作。Wi-Fi 通话保持独立于主机 Cloudflare 域名连接。

数据 APN 按卡保存，只在明确应用时写入数据 CID 1 并读回核验，不自动开启流量、不改 IMS/SOS。部分写入失败保留不确定状态，不自动重试。

### SIP

网页管理账号、固定/共享模块及通话记录；电话由 SIP APP 发起和接听。修改密码、删除账号、注销及权限撤销立即影响对应注册和通话。慢周期查询、呼出记录写入不占用对话锁；过期凭据在查询返回后再次校验。

通话记录查询参数为 `accountId,from,to,before?`，from/to 使用 RFC3339，范围最多 26 小时。每页最多 100 条，返回 nextCursor；完整日期统计返回 outgoingMinutes/incomingMinutes/totalMinutes。网页没有逐条删除通话记录接口，保留期限由服务器配置。

SIP 仅转发 APP 私有音频。蜂窝模块在打开 USB PCM 前仍保留模拟输入静音与读回验证，避免外接麦克风混入；不再通过后台常规轮询反复配置麦克风。具体协议、编解码和实机验收以 `deploy/SIP-CALLS.md` 为准，不以模拟通话代替真实运营商验收。

## 开发者 API 与 Webhook

页面“开发者”按钮打开随安装包发布的文档，示例与本机域名匹配。详细字段与调用示例维护在 `developer-docs.js`，服务端实现在 `developer_api.go`、`webhook.go`。

- `GET /api/v1/modules`：模块、当前号码、cardVersion、能力与异常；不返回 SIM 私密标识。
- `POST /api/v1/attachments`：上传图片/GIF；未使用的临时上传默认保留 2 小时，可在主机设置修改；响应 expiresIn 返回当前有效秒数。
- `POST /api/v1/messages`：指定模块发送 SMS/MMS，支持 requestId 幂等、cardVersion 防旧卡发送和过期时间。
- `GET /api/v1/messages`、`GET /api/v1/messages/:id`、`GET /api/v1/messages/:id/image`：收发及状态同步。
- `POST /api/v1/messages/lookup`：查询 1–100 个不重复 ID，支持发送 ID 与 `rx-` 加 64 位小写十六进制的收件 ID；格式、数量或重复错误返回 `INVALID_BATCH`，缺失消息逐项返回 404。
- `GET /api/v1/events`：事件补拉；过期游标返回 410。事件至少投递一次，接收方按事件 ID 去重。
- Webhook 推送收件、发件状态、模块当前号码/卡激活版本与异常变化；不把短暂读不到卡当成已换卡，不用旧号码替代尚未知的新号码。
- Webhook 独立于 Telegram。仅允许 HTTPS 公网地址，签名保护原始请求体与时间戳；有界重试、持久队列、配置轮换隔离旧目标。
- 普通短信完整正文入库后生成收件事件；按 `hostId + eventId` 去重、资源 `version` 更新。HTTP 200 仅确认事件接收，不代表上游已展示。
- 机器人仅在配置完整且达到对应阈值时发送，内容保持主机、模块、号码、失败、原因。未配置仍记录与展示异常。
- 当前卡任一 SIP 有效响铃/接通、SMS/MMS 发送成功，统一恢复该模块当前卡的 SIP/SMS/MMS 连续计数并取消尚未发送的旧告警。按实际操作时间排序，迟到的旧成功不清除其后的新失败；换卡前结果不影响新卡。硬件异常仍由有效健康检查独立恢复。
- 设置使用 revision 防止另一页面的新配置被覆盖；留空保留密钥，明确清除才删除。密钥轮换不回显旧密钥。

## 自动清理与故障恢复

- 临时上传保留时长由 uploadHours 指定，默认 2 小时，可填 1–8760 的整数；上传被用于消息后，消息拥有独立附件副本，不随暂存副本到期删除。
- 新安装默认 3 天，升级保留已有设置。自动清理 0 天关闭业务历史自动删除，不关闭临时上传到期清理；短信/彩信及其附件、已结束通话、已完成通知与 Webhook 历史统一按填写天数处理。
- 只有一个清理调度入口，每分钟检查，每批最多 256 条；移除通知任务中旧的固定 30 天清理。
- 同步删除留痕、发送请求去重、收件指纹和状态报告按协议保留 7 天；当前异常计数、待发送回调和未恢复结果继续保留，不当作 2 小时临时数据。
- 消息、附件、通话与已完成通知按保留天数处理；未完成发送、下载、正在推送和当前状态不按普通缓存删除。
- 已删除消息保留同步留痕，物理清理后再保留有限期请求去重/收件指纹，避免重试重新投递。
- 配置、账号、会话、卡激活版本、未恢复结果日志和回滚备份不属于 2 小时缓存。
- 发送结果先保存本地持久日志，再写数据库。损坏记录保留并报告健康异常，其他模块继续；不重新发网络请求来“修复”未知结果。

## 验收

按 [测试说明](../deploy/TESTING.md) 分别验证接口、构建产物与实际业务。

## 登录页联系我们

- 主机设置 → 联系我们：填写联系链接，留空隐藏。支持 HTTP(S)、mailto 与 tel；禁止脚本、内嵌凭据和控制字符。
- 配置写入需要登录、同源与 CSRF 校验，revision 防止覆盖其他窗口的设置；公共接口只返回链接，不回传密钥、账号或配置版本。
- 登录页异步读取，失败时隐藏入口，不阻塞登录；外链隔离窗口且不传 referrer。

## 本地软件更新

- `GET /software-update`：已登录管理员读取本机更新状态。
- `POST /software-update/upload/start`：提交 RVU 的 `{size, header, seal}`，先验证签名元数据；header 和 seal 分别为文件头、签名尾的十六进制字符串。
- `POST /software-update/upload/chunk`：`application/octet-stream`，每块最多 1 MiB，通过 `Upload-ID`、`Upload-Offset` 指定已建立的上传与偏移；按服务端确认位置续传。
- `POST /software-update/upload/finish` 或 `/cancel`：正文 `{uploadId}`，完成上传并异步校验，或取消暂存上传。通过 GET 读取校验版本与一次准备票据，上传本身不执行安装。
- `POST /software-update/apply`：正文 `{ticket}`，确认后启动独立安装任务；状态为 queued / running / complete / failed。页面关闭不取消已经提交的安装。
- `GET /version`：当前运行版本；支持域名 `/gly/api` 和局域网 `/api`。
- 仅接受更新包和校验票据，不接受任意命令或服务器文件路径。
- 所有写入保留管理员认证、Origin 和 CSRF 校验。旧整包 ZIP 上传入口已停用；协议与大小上限见 [打包与更新](../deploy/UPDATE-POLICY.md)。

## 模块网络配置

`GET /settings/networks` 返回 networks、modules 和 revision。网络状态是链路配置证据，不代表互联网或运营商已连通。
`PUT /settings/networks` 接收 `{id, revision, modules:[模块ID]}` 替换该网络的分配，或 `{id, revision, label}` 改名。每次仅一种操作；忙碌模块返回 DEVICE_BUSY，过期版本返回 NETWORK_CONFLICT。
绑定持久化；指定网络断开不回退。模块分配不修改 SIP 专用 VPN，主网络切换前将未分配模块固定到原出口。
选中其他网络的模块会在同一事务内移入目标网络；取消当前分配后使用主网络，未设置时使用系统默认网络，不恢复旧分配。

`GET /settings/network-control` 返回主网络、VPN 状态、`routingRevision` 与 `vpnRevision`；`?node=网络ID` 仅向管理员返回节点链接。
`POST /settings/network-control` 使用同一管理员、Origin 与 CSRF 校验：
- 主网络/VPN 开关：`{id, action:"primary"或"vpn", enabled, revision:routingRevision}`。
- 节点保存/检测/移除：`{id, action:"save"或"test"或"remove", revision:vpnRevision, link?, label?}`。

开启新主网络互斥关闭原主网络；模块固定分配保留。具体业务路径、断线回退及 VPN 直连保护见 [网络配置](../deploy/NETWORK-CONFIG.md)。
