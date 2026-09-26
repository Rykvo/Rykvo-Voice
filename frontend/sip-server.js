const SIPServer = (() => {
  let form = null, controller = null, timer = null, status = null;
  const labels = {
    SIP_INVALID_ADDRESS: "请输入有效的 HTTPS 接入地址", SIP_INVALID_INPUT: "请检查接入地址和接入码",
    SIP_INVALID_CODE: "接入码无效", SIP_CODE_BOUND: "接入码已绑定其他主机", SIP_CODE_REVOKED: "接入已撤销",
    SIP_TLS_FAILED: "服务器证书验证失败", SIP_INVALID_CONFIG: "服务器配置不匹配",
    SIP_ROUTE_CONFLICT: "VPN 路由冲突", SIP_INTERFACE_CONFLICT: "VPN 接口已被使用",
    SIP_HANDSHAKE_TIMEOUT: "VPN 握手超时", SIP_HELPER_UNAVAILABLE: "网络服务尚未就绪",
    SIP_RATE_LIMITED: "请稍后重试", SIP_BUSY: "正在配置", SIP_INTERFACE_MISSING: "VPN 尚未启动",
    SIP_NOT_CONFIGURED: "请先填写接入信息", SIP_RECOVERY_REQUIRED: "请重新连接", SIP_ROLLBACK_FAILED: "网络恢复未完成，请检查主机",
  };
  function render() {
    return `<section class="form-page server-page">${ServerNavigation.header("sipServer")}
      <form id="sip-server-form" class="settings-form" novalidate>
        <div class="form-fields">
          ${Forms.field({ prefix: "sip-server", name: "address", label: "接入地址", placeholder: "sip.example.com/api/connect", maxLength: 512, required: true })}
          ${Forms.field({ prefix: "sip-server", name: "accessCode", label: "接入码", type: "password", placeholder: "输入接入码", maxLength: 43 })}
        </div>
        <p class="sip-network-status" role="status" aria-live="polite" hidden></p>
        <div class="form-footer"><button type="submit" class="primary">连接</button><button type="button" data-sip-disconnect class="text-button" hidden>断开</button></div>
      </form></section>`;
  }
  function show(value) {
    status = value;
    const note = form.querySelector('[role="status"]');
    const busy = value.state === "configuring";
    note.textContent = value.issue ? (labels[value.issue] || "连接未完成，请重试")
      : ({configuring:"配置中", connecting:"等待 VPN 握手", connected:"VPN 已连接", disconnected:"未连接", failed:"连接未完成"}[value.state] || "未连接");
    note.dataset.failed = String(Boolean(value.issue) || value.state === "failed");
    note.hidden = false;
    const button = form.querySelector('[type="submit"]');
    button.disabled = busy;
    button.textContent = busy ? "配置中" : value.configured ? "重新连接" : "连接";
    const disconnect = form.querySelector('[data-sip-disconnect]');
    disconnect.hidden = !value.enabled; disconnect.disabled = busy;
    const address = form.elements.namedItem("address");
    if (!address.value && value.address) address.value = value.address;
  }
  async function read(current = form) {
    if (!current || current !== form || controller) return;
    const request = new AbortController(); controller = request;
    try {
      const value = await Backend.sipServer.get({signal:request.signal});
      if (current === form && !request.signal.aborted) show(value);
    } catch (error) {
      if (current === form && !request.signal.aborted) show({state:"failed",issue:error.code});
    } finally {
      if (controller === request) controller = null;
      if (current === form && (status?.enabled || status?.state === "configuring"))
        timer = setTimeout(() => read(current), status?.state === "connected" ? 10000 : 3000);
    }
  }
  async function submit(event) {
    event.preventDefault();
    if (!form || controller) return;
    const current = form, entered = current.elements.namedItem("address").value.trim();
    const address = entered.includes("://") ? entered : "https://" + entered;
    const codeInput = current.elements.namedItem("accessCode"), accessCode = codeInput.value.trim();
    const disconnect = event.type === "click";
    const reconnect = !disconnect && status?.configured && address === status.address && !accessCode;
    let validAddress = false;
    try { const u = new URL(address); validAddress = u.protocol === "https:" && u.pathname === "/api/connect" && !u.username && !u.password && !u.search && !u.hash && (!u.port || u.port === "443"); } catch {}
    if (!disconnect && !reconnect && Forms.report(current, !validAddress ? {field:"address",message:labels.SIP_INVALID_ADDRESS}
      : !/^[A-Za-z0-9_-]{43}$/.test(accessCode) ? {field:"accessCode",message:"请输入完整接入码"} : null)) return;
    clearTimeout(timer);
    const request = new AbortController(); controller = request;
    codeInput.value = "";
    show({...status,state:"configuring",issue:""});
    try {
      const operation = disconnect ? "disconnect" : reconnect ? "reconnect" : "connect";
      await Backend.sipServer[operation]({body:operation === "connect" ? {address,accessCode} : {},signal:request.signal});
    } catch (error) {
      if (form === current && !request.signal.aborted) show({...status,state:"failed",issue:error.code});
    } finally {
      if (controller === request) controller = null;
      if (form === current) timer = setTimeout(() => read(current), 1000);
    }
  }
  function unmount() {
    clearTimeout(timer); timer = null; controller?.abort(); controller = null;
    form?.removeEventListener("submit", submit);
    form?.querySelector('[data-sip-disconnect]')?.removeEventListener("click", submit);
    form?.reset(); form = null; status = null;
  }
  function mount() {
    unmount(); form = document.getElementById("sip-server-form");
    form?.addEventListener("submit", submit);
    form?.querySelector('[data-sip-disconnect]')?.addEventListener("click", submit);
    if (Backend.enabled("sipServer")) read();
  }
  return {render,mount,unmount};
})();
