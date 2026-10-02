const DeveloperDocs = (() => {
  const examples = {
    modules: `curl "$API_BASE/modules" -H "X-API-Key: $API_KEY"`,
    status: `curl "$API_BASE/messages/request-20260928-0001" -H "X-API-Key: $API_KEY"`,
    receive: `curl "$API_BASE/messages?after=0&limit=100" -H "X-API-Key: $API_KEY"`,
    send: `curl "$API_BASE/messages" \\
  -H "X-API-Key: $API_KEY" \\
  -H 'Content-Type: application/json' \\
  --data '{
    "requestId": "request-20260928-0001",
    "moduleId": "module-01",
    "to": "+12025550123",
    "text": "测试短信",
    "expiresIn": 600
  }'`,
    mms: `curl "$API_BASE/messages" \\
  -H "X-API-Key: $API_KEY" \\
  -H 'Content-Type: application/json' \\
  --data '{
    "requestId": "request-20260928-0002",
    "moduleId": "module-01",
    "to": "+12025550123",
    "text": "测试彩信",
    "image": "data:image/gif;base64,BASE64_DATA"
  }'`,
    javascript: `const response = await fetch(process.env.API_BASE + "/messages", {
  method: "POST",
  headers: {
    "Content-Type": "application/json",
    "X-API-Key": process.env.API_KEY
  },
  body: JSON.stringify({
    requestId: crypto.randomUUID(), // 重试时复用这个 ID 和相同正文
    moduleId: "module-01",
    to: "+12025550123",
    text: "测试短信"
  })
});
const result = await response.json();
if (!response.ok) throw new Error(result.error.code);
console.log(result.data.id, result.data.state);`,
    python: `import json, os, urllib.request, uuid

payload = {
    "requestId": str(uuid.uuid4()),  # 超时重试保留这个 payload
    "moduleId": "module-01",
    "to": "+12025550123",
    "text": "测试短信",
}
request = urllib.request.Request(
    os.environ["API_BASE"] + "/messages",
    data=json.dumps(payload).encode(),
    headers={
        "Content-Type": "application/json",
        "X-API-Key": os.environ["API_KEY"],
    },
    method="POST",
)
with urllib.request.urlopen(request, timeout=15) as response:
    print(json.load(response)["data"])`,
    batch: `curl "$API_BASE/messages/batch" \\
  -H "X-API-Key: $API_KEY" \\
  -H 'Content-Type: application/json' \\
  --data '{"items":[
    {"requestId":"batch-request-0001","moduleId":"module-01","to":"+12025550123","text":"测试短信","expiresIn":600},
    {"requestId":"batch-request-0002","moduleId":"module-02","to":"+12025550124","text":"测试短信","expiresIn":600}
  ]}'`,
    lookup: `curl "$API_BASE/messages/lookup" \\
  -H "X-API-Key: $API_KEY" \\
  -H 'Content-Type: application/json' \\
  --data '{"ids":["batch-request-0001","rx-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"]}'`,
    webhook: `{
  "eventId": "123",
  "type": "message.received",
  "version": 123,
  "occurredAt": "2026-09-28T00:00:00Z",
  "hostId": "HOST_ID",
  "host": "HOST_NAME",
  "data": {
    "id": "rx-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    "moduleId": "module-01",
    "cardVersion": 3,
    "number": "+12025550123",
    "mine": false,
    "kind": "sms",
    "text": "测试短信",
    "state": "received"
  }
}`,
    signature: `import hashlib, hmac, time

def verify_webhook(raw_body: bytes, headers, api_key: str) -> bool:
    key_hash = hashlib.sha256(api_key.encode()).hexdigest()
    secret = hashlib.sha256(("rykvo-webhook-v1:" + key_hash).encode()).hexdigest()
    timestamp = headers.get("X-Rykvo-Timestamp", "")
    try:
        if abs(time.time() - int(timestamp)) > 300:
            return False
    except ValueError:
        return False
    signed = timestamp.encode() + b"." + raw_body
    expected = "sha256=" + hmac.new(
        secret.encode(), signed, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(
        expected, headers.get("X-Rykvo-Signature", "")
    )

# 校验通过后，以 hostId + eventId 唯一键持久入库，再返回 2xx。
# 重复事件直接返回 2xx；业务异步处理，不能因重复通知再次发送短信。`,
  };
  const sections = [["start", "开始"], ["send", "发送"], ["receive", "接收"], ["webhook", "通知"], ["batch", "批量"], ["limits", "限额"], ["errors", "错误处理"]];
  let request = null;
  function code(name, title) {
    return `<div class="docs-code"><div><span>${UI.escape(title)}</span><button type="button" class="text-button" data-copy-example="${name}" aria-label="复制${UI.escape(title)}">复制</button></div><pre><code>${UI.escape(examples[name])}</code></pre></div>`;
  }
  function table(headers, rows) {
    return `<div class="docs-table"><table><thead><tr>${headers.map(text => `<th scope="col">${UI.escape(text)}</th>`).join("")}</tr></thead><tbody>${rows.map(row => `<tr>${row.map(text => `<td>${UI.escape(text)}</td>`).join("")}</tr>`).join("")}</tbody></table></div>`;
  }
  function render() {
    return `<section class="developer-docs"><div class="form-toolbar"><button type="button" class="text-button form-back" data-action="developer">‹ 开发者</button></div><header class="docs-header"><h1>API 文档</h1></header><div class="docs-layout"><nav class="docs-nav" aria-label="文档目录">${sections.map(([id, text]) => `<button type="button" data-doc-section="${id}">${text}</button>`).join("")}</nav><div class="docs-content">
      <section id="docs-start">
        <h2>三步接入</h2><ol class="docs-steps"><li>保存 API Key。</li><li>获取模块，发送消息。</li><li>接收通知，查询结果。</li></ol>
        <div class="docs-base"><span>API 地址</span><code data-api-base>https://HOST/api/v1</code></div>
        <p>服务端调用：X-API-Key 鉴权，JSON 请求加 Content-Type: application/json。</p>
        <details><summary>连接与密钥</summary><p>示例变量：API_BASE 为上方地址，API_KEY 为已保存的密钥。外网使用 HTTPS，局域网可用 HTTP。</p><p>密钥至少 6 位，建议 32 位随机字符；仅保存在服务端。清除密钥会关闭 API 和 Webhook。</p></details>
        <details><summary>接口一览</summary>${table(["接口", "用途"], [["GET /modules", "模块、号码、卡版本和状态"], ["POST /messages", "发送短信或彩信"], ["POST /messages/batch", "批量发送：1–25 条"], ["POST /messages/lookup", "批量查询：1–100 个 ID"], ["GET /messages/{id}", "查询单条"], ["GET /messages", "增量同步"], ["POST /attachments", "上传图片"], ["GET /messages/{id}/image", "下载图片"], ["GET /events", "补拉事件"]])}</details>
      </section>
      <section id="docs-send">
        <h2>发送</h2><p>用 /modules 返回的 id 填写 moduleId。</p>${code("modules", "获取模块")}${code("send", "发送短信")}
        <p><strong>202 表示已入队，不代表对方收到。</strong>超时先查询；重试保留相同 requestId 和全部字段，不换 ID 重发。</p>
        <details><summary>查询结果</summary>${code("status", "查询消息")}</details>
        <details><summary>字段</summary>${table(["字段", "说明"], [["requestId", "16–64 位字母、数字、下划线或连字符；每笔业务唯一"], ["moduleId", "模块 id"], ["to", "收件号码，建议包含国家码"], ["text", "短信正文；纯图片彩信可留空"], ["image / attachmentId", "彩信图片，二选一"], ["cardVersion", "可选，指定卡版本，防止换卡后误发"], ["expiresIn", "排队有效秒数：默认 600，最多 86400"]])}</details>
        <details><summary>彩信</summary>${code("mms", "发送彩信")}<p>支持 PNG、JPEG、GIF，原图最多 1 MiB。也可 POST /attachments 提交 {image}，用返回的 id 填 attachmentId。</p></details>
        <details><summary>幂等与保留</summary><p>同 ID、同内容返回 200，不重复发送；同 ID、不同内容返回 409。unknown 表示结果不明，先核实。</p><p>换卡或切换 eSIM 取消旧卡任务，换回不补发。未使用附件默认保留 2 小时，可在主机设置调整。</p><p>记录按主机设置清理，0 为关闭自动清理。清理后的请求 ID 至少保护 7 天；410 REQUEST_EXPIRED 不自动重发。</p></details>
        <details><summary>JavaScript</summary>${code("javascript", "Node.js")}</details><details><summary>Python</summary>${code("python", "Python")}</details>
      </section>
      <section id="docs-receive">
        <h2>接收</h2><p>Webhook 实时通知，增量查询用于首次同步和补漏。</p>
        ${table(["state", "含义"], [["queued / sending", "排队 / 发送中"], ["waiting_network", "等待网络"], ["accepted", "运营商已接收，未确认送达"], ["delivered", "已收到送达回执"], ["received", "已收信"], ["failed / unknown", "失败 / 结果不明"], ["expired / cancelled", "过期 / 取消"]])}
        <details><summary>增量同步</summary>${code("receive", "获取消息")}<p>保存 cursor。more=true：带 after=cursor 和本页 snapshot 继续；more=false：下轮只带 after。</p><p>每页最多 200 条；可按 direction=incoming / outgoing、moduleId 筛选。同一游标不混用筛选条件。</p><p>按消息 id 去重、revision 更新；deleted=true 删除对应展示。410 CURSOR_EXPIRED 从 after=0 重建镜像。</p></details>
        <details><summary>字段与附件</summary><p>收件 id 为 rx- 加 64 位小写十六进制，共 67 位；查询时原样传入。发送 requestId 的 16–64 位限制不适用于收件 id。</p><p>number 是对方号码；本机号码查 /modules。图片按 attachmentPath 下载，仍需 X-API-Key。</p><p>以 carrierAccepted、deliveryConfirmed 区分受理和送达，不依据 displayStatus。receiving / download_pending / downloading 表示接收或下载中。</p></details>
        <details><summary>超时结果</summary><p>排队过期：failed / MESSAGE_EXPIRED。首次发送尝试后 30 分钟仍无明确结果：failed / RESULT_TIMEOUT；后台恢复后继续处理。</p><p>超时不代表未发送，不自动重发。迟到成功按更高 revision 更新为 accepted / delivered；已受理消息不因缺少回执改为失败。</p></details>
      </section>
      <section id="docs-webhook">
        <h2>Webhook</h2><p>填写公开 HTTPS 地址。验签 → 持久保存 → 返回 2xx → 后台处理。</p>
        ${table(["事件", "内容"], [["message.received", "收到短信或彩信"], ["message.status_changed", "状态变化或删除"], ["module.updated", "号码、卡版本、状态和异常"]])}
        <p>按 hostId + eventId 去重；同一资源只应用更高 version。HTTP 200 仅确认接收，不代表上游已展示。</p>
        <details><summary>收件与核实</summary><p>普通短信在完整正文入库后生成收件事件，不先推送空占位正文；真实空短信仍可为空，分片需组装完成。</p><p>需核实时，用 data.id 查询 /messages/{id} 或 /messages/lookup。重复通知、核实失败都不触发重新发送短信。</p></details>
        <details><summary>事件示例</summary>${code("webhook", "收件通知")}<p>换卡使用新的 cardVersion；numberKnown=false 不沿用旧号码。</p></details>
        <details><summary>验签</summary><p>使用原始请求体校验 X-Rykvo-Signature。X-Rykvo-Timestamp 为 Unix 秒，校验时间窗口防重放。</p>${code("signature", "Python 验签")}<p>密钥从 API Key 派生；历史独立签名密钥仍有效，重新保存 API Key 后切换。</p></details>
        <details><summary>重试与补漏</summary><p>超时 10 秒，最多自动尝试 8 次。网络错误、408 / 425 / 429 / 5xx 退避重试；429 / 503 遵守 Retry-After。其他 3xx / 4xx 记失败，不跟随重定向。</p><p>GET /events?after=0 补拉，使用返回的 cursor 继续。事件可能重复或乱序；未推送旧状态会合并，收件不合并。</p><p>修改地址或密钥取消旧配置待推送事件，不重放历史消息；模块快照重新推送。清除配置停止通知，Webhook 不走 Telegram 代理。</p></details>
      </section>
      <section id="docs-batch">
        <h2>批量</h2><p>先检查 /modules 的 features.messageBatch、messageLookup；未声明时使用单条接口。</p>
        ${table(["接口", "请求", "限制"], [["POST /messages/batch", "{items:[消息正文]}", "1–25 条，最多 128 KiB"], ["POST /messages/lookup", "{ids:[消息 ID]}", "1–100 个，不重复"]])}
        <p>查询支持发送 ID 与 67 位收件 ID 混合；HTTP 200 不代表每项成功，逐项检查 data.items。</p>
        <details><summary>查询示例</summary>${code("lookup", "批量查询")}<p>按 requestId 对应原查询 ID，逐项返回 status、data 或 error；不存在的消息为 404 / NOT_FOUND。</p><p>空列表、超限、重复或格式错误的 ID 返回 400 INVALID_BATCH。旧主机拒绝合法收件 ID 时，可改查单条 GET，不重新发送短信。</p></details>
        <details><summary>发送示例</summary>${code("batch", "批量发送")}<p>按主机和密钥分组；彩信先上传，使用 attachmentId，不附带 image。响应逐项含 status、data 或 error、retryAfter。</p><p>超时先查询原 ID；确认缺失后用原 ID、原正文重试，不整批重发。</p><p>按条消耗额度：最多 1,200 条／分钟、突发 25 条，最多 4 个并行批量请求；与单条共用发送总额度。自动回复使用单条接口。</p></details>
      </section>
      <section id="docs-limits">
        <h2>限额</h2><p>每台主机共用。429 / 503 按 Retry-After 等待并降低频率。</p>
        ${table(["请求", "每分钟 / 突发"], [["全部接口", "6,000 / 100"], ["发送", "1,500 / 32"], ["查询与下载", "3,600 / 64"], ["图片上传", "300 / 4"], ["并行处理", "16 个请求，图片最多 4 个"]])}
        <details><summary>队列与并发</summary>${table(["项目", "上限"], [["后台任务", "64 路，同模块串行；不是每秒 64 条"], ["单模块提交", "10 条／分钟，网页与 API 合计"], ["发送队列", "每模块 100 条，全机 2,000 条"], ["临时附件", "256 个，每张最多 1 MiB"]])}<p>分类额度与总额度同时生效。内联 image 占图片上传额度，attachmentId 不重复占用。后台并发不等于运营商吞吐。</p></details>
      </section>
      <section id="docs-errors">
        <h2>错误处理</h2>${table(["错误", "处理"], [["400 INVALID_BATCH", "检查数量、ID 格式与重复项"], ["401 API_UNAUTHENTICATED", "检查 API Key"], ["429 API_RATE_LIMIT / MESSAGE_RATE_LIMIT", "按 Retry-After 降速重试"], ["503 API_BUSY / SERVICE_DRAINING", "等待重试，保留原 requestId"], ["409 REQUEST_CONFLICT", "同 ID 内容不同，核对原请求"], ["409 DEVICE_CHANGED", "刷新模块与卡版本"], ["410 REQUEST_EXPIRED", "核实原任务，不自动重发"]])}
        <details><summary>其他错误</summary>${table(["错误", "处理"], [["403 HTTPS_REQUIRED", "外网使用 HTTPS"], ["400 INVALID_MESSAGE / INVALID_IMAGE", "检查字段、格式与大小"], ["404 ATTACHMENT_EXPIRED", "重新上传未入队的附件"], ["429 MESSAGE_QUEUE_FULL / UPLOAD_QUEUE_FULL", "等待队列释放"], ["410 CURSOR_EXPIRED", "从 after=0 重新同步"], ["503 DATABASE_UNAVAILABLE", "稍后查询原 ID，不换 ID 重发"]])}</details>
        <p>API 不含拨号、挂机、重启和账号管理；SIP 使用独立账号。</p>
      </section>
    </div></div></section>`;
  }

  async function mount() {
    request?.abort(); request = new AbortController();
    const controller = request;
    if (typeof Backend === "undefined" || !Backend.enabled("developer")) return;
    try {
      const data = await Backend.developer.get({ signal: controller.signal });
      if (request !== controller || controller.signal.aborted) return;
      const node = document.querySelector("[data-api-base]");
      if (node && data.apiBase) node.textContent = data.apiBase;
    } catch { /* 文档本身不依赖配置请求。 */ }
  }
  function unmount() { request?.abort(); request = null; }
  document.addEventListener("click", async event => {
    const section = event.target.closest("[data-doc-section]");
    if (section) document.getElementById(`docs-${section.dataset.docSection}`)?.scrollIntoView({ block: "start" });
    const copy = event.target.closest("[data-copy-example]");
    if (!copy || !Object.hasOwn(examples, copy.dataset.copyExample)) return;
    try { await navigator.clipboard.writeText(examples[copy.dataset.copyExample]); UI.toast("已复制"); }
    catch { UI.toast("请选中代码复制"); }
  });
  return { render, mount, unmount };
})();
