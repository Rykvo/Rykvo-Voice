# 后端对接

## 交付边界

页面、交互和统一接口客户端已保留。登录、管理员修改、独立密码、功能显示及 Cloudflare Tunnel 已实现 Go + PostgreSQL（`outputs/rykvo-backend/`），其余业务后端、SIP 栈、短信网关及媒体服务尚未实现。

`http.js` 统一处理访问前缀：局域网 `/api`，域名 `/gly/api`；下表按无前缀路径说明。服务端为域名入口注入 `<base href="/gly/">`，登录跳转、静态资源及事件订阅共用该入口。

`api.js` 提供全部功能的调用入口。管理员、登录、功能显示与云服务器页面已使用该入口；其他页面当前读取本地状态，仍需在下表所列位置绑定请求结果与事件。仅注入配置或实现 URL，不会自动把所有预览动作变成真实通信。接入时保留现有渲染与样式，不另造一套页面。

## 启用与调用

默认启用 `session-api`（登录、管理员、功能显示）和 `tunnel-api`（Cloudflare Tunnel），其余业务不自动发送接口请求。完整业务接入后由服务端在 HTML head 注入：

```html
<meta name="backend-api" content="enabled" />
```

仅接入云服务器时保留 `tunnel-api` 开关即可，见 `TUNNEL.md`。生产使用 HTTPS、同源 `/api` 与 HttpOnly/Secure/SameSite 会话 Cookie；不要把管理令牌放在 URL、源码或 localStorage。

```js
const session = await Backend.session.get();
Backend.setCSRFToken(session.csrfToken);
const controller = new AbortController();
const records = await Backend.callRecords.list({
  query: {
    sipAccountId: accountId,
    direction: "outgoing",
    from: startOfDayMs,
    to: startOfNextDayMs,
    timeZone: "Asia/Shanghai",
    limit: 100,
  },
  signal: controller.signal,
});
// 页面离开时取消读取；取消请求不代表撤销服务器上的操作。
controller.abort();
```

调用选项统一为 `{ params, query, body, signal, idempotencyKey }`。路径 ID 使用字符串，由客户端编码。读取请求不接收 body。文件上传用 FormData，其他 body 用 JSON。

客户端包含 15 秒超时、同源 Cookie、禁止重定向、no-store、可取消请求与写入幂等键；不自动重试写入。超时先查询实际结果，确需重试时复用同一 `idempotencyKey`，后端必须实现去重。CSRF 令牌仅存内存；退出后清空并关闭订阅。

成功响应：`{ "data": ... }`；无内容使用 204。兼容云服务器原有直接 JSON 响应。分页统一为：

```json
{ "data": { "items": [], "nextCursor": null, "total": 0 } }
```

失败使用正确 HTTP 状态及 `{ "error": { "code": "INVALID_FIELD", "field": "username" } }`。支持 400/401/403/404/409/422/429/500；不把密钥、原始堆栈、设备命令输出放入错误响应。客户端错误含 `code/status/field`，表单用 `Forms.report` 显示，失败保留输入。

## 登录对接

`auth.js` 调用 `Backend.session.get/login/logout`。默认 HTML 的 `session-api=enabled` 仅启用认证、入口计数及功能显示配置，不启用未完成的业务接口。

- 页面加载先 GET `/api/session`：有效会话显示应用；401 显示登录表单。
- POST `/api/session`：`{username,password}`，成功返回 `{data:{user:{id,username},csrfToken,expiresAt}}` 并设置 HttpOnly Cookie。
- DELETE `/api/session`：同源 Origin 与 CSRF 令牌，成功返回 204，前端刷新回登录页。
- 登录必须返回非空字符串 `user.id`、`csrfToken`，无有效响应不进入应用。不在前端验证默认密码。
- 后端每次查询接口都验证会话；隐藏界面不是访问控制。当前未实现的业务接口保持 503。
- 密码使用 PBKDF2-HMAC-SHA256（600,000 次）和独立随机盐，哈希存 PostgreSQL；初始化不覆盖已有账号。
- 当前管理员资料修改接口仍未接入，页面不会假装保存成功。

## 功能显示保护

- POST `/ui/activation`：`{reset:false}` 上报一次图标点击；返回 `{challenge:boolean}`。计数仅在服务端，登录会话隔离；点击别处发送 `{reset:true}`。
- GET `/settings/visibility`：已登录可读 `{features:对象或null}`，用于页面显示；null 时保留原本地显示偏好，首次授权保存时迁移。
- POST `/settings/visibility/unlock`：`{password}`。校验独立密码，成功返回 `{verified:true}`，在数据库当前登录会话记录 5 分钟授权。
- GET `/settings/visibility/access`：验证当前授权，无授权返回 403 `VERIFICATION_REQUIRED`。
- DELETE `/settings/visibility/access`：关闭窗口撤销当前授权，返回 204。
- PUT `/settings/visibility`：`{features:{功能ID:boolean}}`，只接受已知功能；需要独立授权。合并保存账户级显示偏好，返回完整 `{features}`。不调用停止服务接口。
- PUT `/settings/visibility/password`：`{currentPassword,newPassword}`，需要独立授权并重新核对当前独立密码；成功返回 204，并撤销全部独立授权。登录密码不变。
- 独立密码使用随机盐和 PBKDF2-HMAC-SHA256，600,000 次。限流保存在数据库，同一登录账号 15 分钟最多 5 次失败，成功验证重置；重启或换 IP 不重置失败限制。
- 初始化通过服务端 `-init-visibility` 从标准输入读取初始密码，不放源码、不覆盖已有密码。
- UI 显示不是业务权限。前端监听仍可分析；服务器保护密码校验、修改与配置写入，不承诺隐藏所有前端代码。当前虚拟机 HTTP 保留，HTTPS 后续配置。

## 接口清单

下列路径均以 `/api` 为前缀。列表通用参数：`cursor/limit/search`；具体过滤参数见字段约定。

| 客户端方法                   | HTTP                 | 路径                                      |
| ---------------------------- | -------------------- | ----------------------------------------- |
| session.get / login / logout | GET / POST / DELETE  | /session                                  |
| modules.list                 | GET                  | /modules                                  |
| modules.get / update         | GET / PATCH          | /modules/:moduleId                        |
| modules.lines                | GET                  | /modules/:moduleId/lines                  |
| modules.updateLine           | PATCH                | /modules/:moduleId/lines/:lineId          |
| modules.installESIM          | POST                 | /modules/:moduleId/esim                   |
| calls.list / dial            | GET / POST           | /calls                                    |
| calls.get                    | GET                  | /calls/:callId                            |
| calls.answer                 | POST                 | /calls/:callId/answer                     |
| calls.reject                 | POST                 | /calls/:callId/reject                     |
| calls.hangup                 | POST                 | /calls/:callId/hangup                     |
| calls.dtmf                   | POST                 | /calls/:callId/dtmf                       |
| calls.media                  | POST                 | /calls/:callId/media                      |
| calls.ice                    | POST                 | /calls/:callId/ice                        |
| callRecords.list             | GET                  | /call-records                             |
| callRecords.stats            | GET                  | /call-records/stats                       |
| callRecords.remove           | DELETE               | /call-records/:recordId                   |
| callRecords.removeAll        | POST                 | /call-records/delete                      |
| conversations.list           | GET                  | /conversations                            |
| conversations.get / remove   | GET / DELETE         | /conversations/:conversationId            |
| conversations.messages       | GET                  | /conversations/:conversationId/messages   |
| conversations.markRead       | POST                 | /conversations/:conversationId/read       |
| conversations.removeAll      | POST                 | /conversations/delete                     |
| messages.send                | POST                 | /messages                                 |
| messages.get                 | GET                  | /messages/:messageId                      |
| attachments.upload           | POST                 | /attachments                              |
| attachments.remove           | DELETE               | /attachments/:attachmentId                |
| sip.list / create            | GET / POST           | /sip/accounts                             |
| sip.get / update / remove    | GET / PATCH / DELETE | /sip/accounts/:accountId                  |
| administrator.get / update   | GET / PATCH          | /administrator                            |
| developer.get / update       | GET / PATCH          | /settings/developer                       |
| retention.get / update       | GET / PUT            | /settings/retention                       |
| preferences.get / update     | GET / PATCH          | /settings/preferences                     |
| visibility.get / update      | GET / PUT            | /settings/visibility                      |
| updates.get                  | GET                  | /updates                                  |
| updates.check                | POST                 | /updates/check                            |
| updates.install              | POST                 | /updates/install                          |
| jobs.get                     | GET                  | /jobs/:jobId                              |
| tunnel.get                   | GET                  | /tunnel                                   |
| tunnel.connect               | POST                 | /tunnel/connect                           |
| tunnel.disconnect            | POST                 | /tunnel/disconnect                        |
| subscribe                    | SSE                  | /events                                   |

## 字段与业务规则

### 会话与设备

- 登录请求 `{username,password}`；会话响应 `{user:{id,username},csrfToken,expiresAt}`。注销撤销服务端会话。
- 模块 `{id,name,label,number,status,signal,sims:[]}`。`status` 为 online/offline/error；`signal` 为 none/wifi/mobile/unicom/telecom。当前模块 ID 使用 `module-数字`；若换成任意 ID，同时调整 `Lines.recorded` 校验及迁移，不能静默换线。
- SIM `{id,label,number,enabled,wifiCalling,roaming}`，操作绑定稳定 lineId，而不是页面数组下标。
- 模块 PATCH `{label}`；线路 PATCH 仅包含修改字段。eSIM 启停、标签和删除等待设备确认。Wi-Fi 通话使用同一路线 PATCH，字段为 {requestId,wifiCalling}，返回更新后的 Module；漫游控制仍待接入。手动搜网与选网已移除，网络状态由后台只读采集。
- eSIM 请求与任务格式见下方“eSIM 实接接口”；激活码不落浏览器存储，后端负责安装与进度。

### 电话、记录与统计

- 拨号 `{number,moduleId,lineId?,sipAccountId?}`。`moduleId:"random"` 表示后端从在线且启用的允许模块中原子选择，响应必须带实际 moduleId/lineId。
- 实时通话 `{id,number,moduleId,lineId,sipAccountId,direction,phase,startedAt,connectedAt,endedAt}`；phase 为 dialing/ringing/connected/ended/failed。answer/reject/hangup 为显式命令。
- DTMF `{digits}`；media 为 WebRTC SDP `{type:"offer"|"answer",sdp}`，ICE `{candidate,sdpMid,sdpMLineIndex}`。返回协商结果与必要 ICE 配置。浏览器媒体权限、RTCPeerConnection、音频输出尚待接入，不把 HTTP 成功当成接通。
- 历史记录 `{id,number,at,duration,direction,status,moduleId,lineId?,sipAccountId?,kind?}`。`at` 为呼叫开始毫秒时间戳；`duration` 为实际接通秒数。status 为 connected/unconnected/missed/busy/cancelled/failed。
- 旧电话列表按 kind 显示：已接来电 incoming、接通呼出 outgoing、未接来电 missed、未接通呼出 cancelled。接入适配时从 direction/status 映射，SIP 记录仍保留原 status。
- `list/stats` 使用同一过滤范围 `{sipAccountId,direction,from,to,timeZone}`，时间范围 `[from,to)`；以指定时区的当天零点至次日零点计算，不固定加 24 小时，以兼容夏令时。
- SIP 只统计当前账号的呼出。每通 connected 使用 `max(1,ceil(duration/60))`，其他为 0；逐通进位后相加。stats 返回 `{total,connected,unconnected,billedMinutes}`；总计覆盖全部分页，不对当前页求总计、不先汇总秒再取整。
- 单条记录不合并，按 at/id 降序稳定分页。批量删除请求 `{ids:[...]}`，全删为 `{all:true,filter:{...}}`；后端先验证账号范围，前端仍需删除确认。删除记录不发挂机命令。

### 信息与附件

- 会话 `{id,number,region?,unread,senderId,lastMessage,updatedAt}`；详情按需返回消息，避免列表加载全部正文。
- 消息 `{id,conversationId,text,mine,at,senderId,status,attachmentIds:[]}`。status 为 queued/sent/delivered/failed；区分已入队与已送达。
- 发送 `{conversationId?,number?,senderId,text,attachmentIds:[]}`，新会话允许 senderId 为 random，响应返回实际固定线路；已有会话沿用原线路，失效时提示，不擅自换线路。
- 图片先 `attachments.upload({body:formData})`，字段 file；响应 `{id,url,mime,size}`。服务端校验 MIME、体积和权限，返回受权限保护的同源地址。生产不把大图 data URL 存入 localStorage。
- 标记已读 `{throughMessageId}`；删除会话/批量删除与删除消息附件关联由服务端事务处理，不影响其他会话引用的附件。

### SIP、设置与维护

- SIP 创建 `{username,password,port}`，端口整数 1–65535，默认 5060；服务器 IP 由服务端配置返回，不由创建表单填写。
- SIP 账号 `{id,ip,username,port,hasPassword,allocation,moduleIds,receiveCalls}`；列表显示 IP:端口（IPv6 加方括号），模块列只显示“全部/固定”。
- 更新允许 `{username,password?,port,allocation:"all"|"fixed",moduleIds:[],receiveCalls}`。固定至少选一个有效且有权使用的模块。receiveCalls 关闭只拒绝该账号入站，不禁用呼出。
- 不把密码掩码当真实密码回传；密码未修改时省略 password。生产列表返回 hasPassword，不返回明文；当前预览依赖内存密码，接入时相应调整 `sip.js` 的字段读取/校验。
- 管理员 GET 返回 `{account}`；PATCH 提交 `{account,currentPassword,newPassword,confirmPassword}`，空账号或新密码保留原值。服务端验证当前登录密码、长度与确认密码，成功返回 204 并撤销该账号全部会话，前端重新载入登录页。每账号 15 分钟最多 5 次修改尝试；独立密码不受影响。
- 开发者配置 `{username,apiKey?,webhook,botToken?,adminId,notificationId,telegramProxy?}`。ID 保持字符串。GET 返回非敏感字段和 hasApiKey/hasBotToken/hasTelegramProxy；不回显秘密。PATCH 省略表示不改，显式 null 表示清除。代理仅用于 Telegram，后端解析 `IP:端口:账号:密码`，出站 URL/代理地址需后端限制目标，防止 SSRF。
- 保留规则 `{days}`，0–36500 整数，0 关闭。服务端按真实时间清理所有账户范围内的过期通话/消息/孤立附件，但保留账号、设置及近期消息。后端调度不依赖网页打开；前端本地清理不应在服务模式代替服务端删除。
- 偏好 `{theme:"light"|"dark",motion:boolean}`；motion 为减少动态效果。
- 显示配置为 `{modules,phone,messages,general,administrator,sip,server,developer,cleanup,updates}` 布尔值；只控制显示。general 关闭隐藏子入口，保留各子开关原值。管理权限必须服务端校验，隐藏入口和十次点击不是认证机制。
- 更新查询 `{currentVersion,availableVersion,status}`；检查可返回 `{jobId}`，安装 `{version}`，须先确认并验证发布包签名；任务 `{id,status,progress,errorCode?}`。前端不自行下载执行未知脚本。

## 实时事件

```js
const unsubscribe = Backend.subscribe((event) => {
  // 按 id/version 去重；只更新受影响的节点，不重建整页。
  // 切换显示开关时不要取消全局通话/消息服务订阅。
});
// 退出账户或停止该订阅时调用；最后一个订阅取消后关闭连接。
unsubscribe();
```

SSE 使用标准 message 事件，`id:` 为事件游标，`data:` 为 `{id,type,at,version,data}`。type 建议 module.updated / line.updated / call.updated / call-record.created / message.created / message.updated / conversation.updated / sip.updated / settings.updated / job.updated。data 含稳定资源 ID 及改变字段。

后端按会话权限过滤事件，支持 Last-Event-ID 续传。客户端重新连接或检测到版本缺口时重新拉取相关资源；服务端保留去重窗口。当前客户端负责共享连接、解析及退订，业务版本去重与缺口补取仍由接入层实现。

## 逐页绑定位置

| 文件 / 位置                                            | 接入工作                                                             |
| ------------------------------------------------------ | -------------------------------------------------------------------- |
| `module-data.js` items；`modules.js` render/updateRows | 拉取模块，保存标签后合并返回数据；重新计算统计；不要保留空数组快照   |
| `cellular.js` change / submit                          | 线路开关和 eSIM 改为等待接口结果；失败回滚                 |
| `phone.js` toggleCall / renderHistory                  | 替换 1300ms 模拟接通；由真实事件驱动状态；挂断记录以后端为准         |
| `messages.js` send / showThread / removeThreads        | 拉会话和分页消息；接口确认后更新消息状态，失败保留草稿；复用增量插入 |
| `sip.js` submit / delete / formValues                  | 账号列表与 CRUD 接口，处理 hasPassword，保存固定模块及接电话配置     |
| `sip-history.js` calls / update                        | 按账号、日期请求分页列表和 stats；总时长取完整范围统计               |
| `admin.js`、`developer.js` submit                      | 替换未连接提示为接口请求，加载非敏感字段；提交成功再提示             |
| `cleanup.js` days / submit                             | 服务模式保存 retention；不使用本地结果宣称服务端已清理               |
| `visibility.js` read / change                          | 已绑定数据库显示配置与独立密码授权；仅缓存显示标志，不缓存密码       |
| `app.js` applyPreferences / updates                    | 偏好同步与更新查询、确认安装、任务进度                               |
| `server.js` request                                    | 已使用 Backend.tunnel；启用后按 `TUNNEL.md` 联调                     |

登录界面与会话过期已实现；业务接入仍需补充真实媒体链路和事件适配，不属于这次静态前端交付。未完成接入前不要启用生产通信。

## 验收

- 所有列表空状态、分页、搜索、窄屏、深浅色保持现有布局；账号 A 看不到账号 B 的记录。
- 保存失败不显示成功；断网/401/403/409/429、超时结果不明均有明确处理。
- 快速重复点击不会重复拨号、发消息或创建账号；幂等重试不重复扣费。
- 来电开关不影响呼出，显示开关不停止正在运行的业务。
- 日期边界与 1/59/60/61 秒计费、跨页总计一致；清理 0 天不删数据。
- SSE 重连不丢消息、不重复渲染；页面卸载取消读请求和局部订阅，无重复监听和无效定时器。

## 页面访问隔离

Go 使用 `WEB_ROOT` 指定发布目录。`/` 按有效 Cookie 返回登录页或主界面。登录仅引用公共基础样式、表单控件、请求工具与登录脚本；业务 JS/CSS 和图标必须带有效会话，未登录返回 401。两个 HTML 的直接地址跳回 `/`。禁止测试、文档、源码映射及目录列表；页面和资源使用 `private, no-store`。Nginx 全部转发 Go，不公开静态目录。已发送到浏览器的资源仍可被查看或保存，此隔离不等于代码加密。

## 模块接入（1.1.0）

模块已接入自动发现、只读采集、稳定绑定和唯一标签；无需手动添加，不设数量配额。列表返回 `{data:{items:[],discoveryIssue:""}}`，详情返回 `{data:Module}`。Module 增加 `managed/labelCustom/kind/hardware/issue/capabilities`；signal 支持 cellular，运营商和信号数值使用 hardware。eSIM 卡内标签、启停、安装与删除已实现接口，待实机联调；手动搜网和选网已移除；Wi-Fi 通话已接入 EC20 飞行模式与 IMS 注册/续期；数据连接及漫游、短信、实际呼叫和音频仍待接入。

标签 PATCH 仍使用 `{label}`，空值恢复可用默认标签；旧标签迁移可带 `ifUnmodified:true`。标签唯一性包含离线记录，不把同一模块原标签视为重复。浏览器共享一个单飞轮询，隐藏时取消请求；本阶段不启用预留的全业务 SSE。详见 [模块后端](../backend/MODULES.md)。

### eSIM 实接接口

- `POST /modules/:moduleId/esim`：`{requestId,eid,activation,confirmation?,imei?}`，下载并安装，不自动启用。模块可读取 IMEI 时使用真实读数；读卡器需提供目标设备 IMEI。
- `PATCH /modules/:moduleId/lines/:lineId`：`{requestId,eid,label}` 或 `{requestId,eid,enabled}`，修改卡内昵称或启停配置。
- `DELETE /modules/:moduleId/lines/:lineId`：`{requestId,eid}`，只删除停用且策略允许的配置。
- `POST /modules/:moduleId/esim/notifications`：`{requestId,eid}`，重试运营商通知；成功送达后才移除卡内通知。

写请求返回 202 和任务 `{id,action,state,stage,issue,warning}`，不是操作成功。沿用模块列表单一轮询读取 `Module.job`。同一 requestId 不重复写卡；重启后的未完成任务标记 uncertain，不自动重放。激活码和确认码不存数据库、浏览器存储或日志。

`hardware.esim` 含 EID、配置、待发送通知数；识别成功才启用 `capabilities.esim`。配置的 `id` 由 EID + ICCID 生成，`canDisable/canDelete` 反映卡片策略。普通 SIM 不开放 eSIM 控制，短信/通话/射频设置仍未接入。UI 只在任务完成且重新读卡核实后显示真实状态。

普通 SIM 和每个 eSIM 配置分别返回 `iccid` 与 `number`。号码未知时保持 `number` 为空，页面改显示带 ICCID 标识的卡号；ICCID 不进入拨号、短信号码格式化或号码字段。未启用配置照常列出，不借用当前启用配置的号码。模块列表可按 ICCID 搜索。

当前号码来源为模块 `AT+CNUM`。VoCat 参考项目另有 Own Numbers、EF_MSISDN 读取，以及 IMS 注册后由运营商返回关联号码的链路（[读取流程](https://github.com/MengMengCode/VoCat/blob/484cd236dd543e2ba142cf1da2c5808ee8e89a6e/internal/device/phone.go)、[IMS 关联号码](https://github.com/MengMengCode/VoCat/blob/484cd236dd543e2ba142cf1da2c5808ee8e89a6e/internal/vowifi/phone.go)）。这些链路尚未接入本项目；缺少本机号码不等于蜂窝注册失败，也不证明必须启用 Wi-Fi 通话。

EC20 连续通信超时恢复期间 `issue=RECOVERING`，页面显示“正在恢复”。不增加浏览器计时器、重启接口或固定成功提示；恢复状态由原模块列表轮询更新。
# APN 接入

- `GET /modules/:moduleId/lines/:lineId/apns`：当前卡已保存配置与模块当前 PDP 上下文。读取忙碌以 `issue` 返回，不虚构“使用中”。
- `PUT /modules/:moduleId/lines/:lineId/apns/:apnId`：按 ICCID 保存 `apn,protocol,auth,username,password`，可用 `preservePassword` 保留已有密码。列表只返回 `hasPassword`，不回传密码。
- `DELETE /modules/:moduleId/lines/:lineId/apns/:apnId`：删除已保存配置，不删除模块上下文。
- `POST /modules/:moduleId/lines/:lineId/apns/:apnId/apply`：明确应用已保存配置至数据 CID 1；需当前卡匹配、设备空闲、无活动数据连接。写后读回 APN 与协议；不发送附着、PDP 激活或重启命令，不修改 IMS/SOS。部分写入失败返回 `APN_APPLY_UNCONFIRMED`，需人工核实，禁止自动重试。

蜂窝数据入口位于 SIM 详情的“数据漫游”下方。与 Wi-Fi 通话的 IMS APN 独立，不复用用户的数据 APN。

## SIP 电话服务器网络接入（1.5.28）

- 独立 `sip-network-api` scope，不修改主机服务器 `/tunnel`。
- `GET /settings/sip-server` 返回 VPN 状态、公开绑定参数；从不返回私钥、接入码。
- `POST /settings/sip-server/connect` 接受 `{address,accessCode}`；地址必须是 HTTPS `/api/connect`。
- `POST /settings/sip-server/reconnect`、`POST /settings/sip-server/disconnect` 接受 `{}`，复用已保存配置。
- 写操作要求现有管理员会话、同源 Origin、CSRF；返回 202 只表示受理。GET 真实 WireGuard 握手有效时才显示 `VPN 已连接`。
- 云端按持久化 installationId 领取配置；本机只解析密钥和网络参数，不执行云端路由钩子。凭据保存在 root-only 状态目录，不缓存接入码。
- 状态 `network` 包含 interface、bindAddress、publicAddress、server、start、end，预留给后续本地电话引擎；`capabilities.calls=false`。
- 后续 SIP/RTP 绑定 bindAddress，端口均在 start–end 内；对外 SDP 地址用 publicAddress。当前不创建 SIP 账号、不开放电话监听、不拨号。
- 主机默认路由、DNS、Cloudflare Tunnel、运营商 IMS/MMS 路由保持独立。
- `POST /modules/:moduleId/lines/:lineId/emergency-address/session` 紧急联系地址仍为预留接口，不启用其 scope，不发送请求。

## 服务器入口布局修正
通用仅保留 `server`（“服务器”）入口。`server`/`sipServer` 路由分别对应同一设置页面的“主机服务器”和“SIP 电话服务器”切换项，沿用原挂载/卸载流程，不改变 Tunnel 连接。两者共用 `server` 显示设置；旧 `sipServer` 显示键仅由后端保留兼容，不再展示独立开关。切换离开 SIP 表单会清除接入码。紧急地址入口仍为预留；SIP 网络接口见上节。

## v1.4.4：数据漫游偏好与 Wi-Fi 互锁
- 当前线路 `PATCH /modules/:moduleId/lines/:lineId` 新增独立请求 `{roaming:boolean,requestId:UUID}`，不可与 Wi-Fi/标签/eSIM 开关混合提交。
- 按当前已核实 SIM 的 ICCID 持久保存，换卡隔离、重启后载入；旧请求重放不会覆盖新选择。继续使用管理员会话、Origin 与 CSRF 校验。
- Wi-Fi 开关开启（含连接中、断线等待、失败）或仍在关闭清理时，前端将漫游开关置灰，后台拒绝修改；完整关闭后恢复可操作。置灰不清除原有选择。
- 本字段是数据使用许可，不会直接发起 PDP、打开射频、改变默认路由或启用主机流量；实际数据/MMS 传输仍待接入并消费该许可。SMS/MMS 收发尚未实现。

### 重启
- `POST /modules/:moduleId/restart`：独立模块重启。
- `POST /modules/restart`：全部 USB 模块依次重启，不受列表筛选影响。
- `POST /modules/host-restart`：主机延迟重启。
- 请求 `{requestId, confirm: true}`；登录、同源、CSRF 与持久化去重。设备忙时拒绝，不中断 eSIM 写入或发送中的消息。
- 模块响应 `{jobs: [{moduleId, job}]}`；相同 IMEI 的新 USB 代次读取正常后任务才完成。Wi-Fi 通话意图和漫游设置不修改。
- 主机响应 `{state: "accepted" | "uncertain"}`；仅报告已提交或结果待确认，不把 HTTP 202 当作恢复完成。不自动重放重启。


## 信息备注与会话显示

- `PUT /messages/contacts`：`{lineId, number, name}`，同源登录与 CSRF 验证，备注最多 24 个 Unicode 字符；空备注恢复号码。存入主机数据库，不依赖浏览器缓存。
- `GET /messages` 同时返回 `contacts: [{lineId, number, name, revision}]`；客户端仅接受较新备注版本，保留清空记录以防旧备注复活。
- 会话按原发送模块/SIM 隔离，使用同一 SIM 已出现的国际号码或已保存的备注号码解析无加号、本地格式。英国 +44 同时识别 0 前缀。仅有本地号码或匹配多个国家时不猜测、不合并；短号和字母发件人独立。原始消息、线路绑定不变。
- 已接收的短信不显示发送状态。排队/发送/等待网络显示转圈；只有真实 `delivered` 回执显示“已送达”；accepted/unknown/partial/failed 等显示“尚未送达”，不代表可以安全重复发送。
