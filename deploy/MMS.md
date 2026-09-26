# EC20 彩信

## 通道

- 蜂窝发送：按当前 SIM 的运营商配置选择 MMS APN、鉴权、MMSC 和代理；独占模块 AT 通道，核对设备世代、IMEI、ICCID。
- 使用空闲匹配 PDP 或未定义的 CID 4–16。不改默认数据、IMS/SOS、主机路由或已激活的通道；仅释放本次创建/激活的资源。
- `QICSGP → QIACT → QMMSCFG → QMMSEDIT → QMMSEND`。附件放 RAM，上传校验通过后才提交；只清理本次文件。
- 普通附件使用 QFUPL ACK 模式及大小/XOR 校验。包含 `+++` 的附件按 QFWRITE 分块写入并完整回读，避免逃逸数据模式；不修改图片内容。
- 只有最终 `+QMMSEND: 0,200` 表示彩信中心接受。初始 OK 不是发送结果，也不等于对方收到。
- 已开始提交但超时/断开：结果待确认，禁止自动重发。提交前失败：明确失败。缺少网络/配置：等待提示，不显示发送转圈。

## 接收

EC20 没有原生 QMMSRECV。现有 WAP Push 短信解码/分片重组提取通知，在同一 SIM 的 MMS PDP 上通过 QIOPEN/QISEND/QIRD 获取 MM1 内容。代理使用标准 HTTP absolute-form 请求，不走主机私网。HTTP 分帧和二进制长度有边界；正文/图片入库后才发 M-NotifyResp.ind，确认失败不撤销已收到的内容。

## 当前边界

- 需要蜂窝数据注册及运营商彩信业务；漫游遵守该 SIM 的数据漫游开关。
- 不自动关闭 Wi-Fi Calling。Wi-Fi 会话启用时保留原主机 HTTP 通道，私有彩信网关仍需蜂窝承载，等待状态不代表适配完成。
- 原生路径当前只支持 HTTP MMSC；不把 HTTPS 静默降级。通知 URL 限定与 MMSC 同源，不跟随重定向。不同域名的运营商下载地址需核实后单独适配。
- 附件限于已验证的文字和 PNG/JPEG/GIF；运营商大小限制、套餐限制和最终投递仍须实卡验证。只有真实投递回执才可标记已送达。
- 模块超时后不继续发送可能被当作数据的清理命令；此时保留 RAM 临时文件，避免干扰未完成的模块命令。

## 验证

默认测试不接触设备。显式设置 `RYKVO_MMS_FILE_TEST_PORT` 后，`TestMMSModemFileRoundTrip` 只验证专用 RAM 测试文件，不激活 PDP、不发送彩信；测试文件已存在时中止。可额外设置 RYKVO_MMS_FILE_TEST_INPUT 读取本机附件进行原字节回读验证，不记录内容、不提交运营商。

参考：[Quectel MMS 应用说明](https://forums.quectel.com/uploads/short-url/1dHDGInW30kRmgeYRnDwzmqzz3t.pdf)、[文件接口](https://forums.quectel.com/uploads/short-url/9qrEyTIpmnu6obn9OaohxYOhPRi.pdf)、[EC20 TCP/IP](https://forums.quectel.com/uploads/short-url/6VIC0qyhQEFIEOSJROGSMbqezTq.pdf)。
