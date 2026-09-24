const SIPServer = (() => {
  let form = null, controller = null;
  function render() {
    return `<section class="form-page server-page">${Forms.header("SIP 电话服务器", "sip.svg")}
      <form id="sip-server-form" class="settings-form" novalidate>
        <div class="form-fields">
          ${Forms.field({ prefix: "sip-server", name: "address", label: "接入地址", placeholder: "sip.example.com:5061", maxLength: 512, required: true })}
          ${Forms.field({ prefix: "sip-server", name: "accessCode", label: "接入码", type: "password", placeholder: "输入接入码", maxLength: 512, required: true })}
        </div>
        <p class="server-error" role="status" aria-live="polite" hidden></p>
        <div class="form-footer"><button type="submit" class="primary">连接</button></div>
      </form></section>`;
  }
  async function connect(event) {
    event.preventDefault();
    if (!form || controller) return;
    const current = form;
    const address = current.elements.namedItem("address").value.trim();
    const codeInput = current.elements.namedItem("accessCode");
    const accessCode = codeInput.value;
    if (Forms.report(current, !address || /\s/.test(address)
      ? { field: "address", message: "请输入接入地址，不含空格" }
      : !accessCode.trim() ? { field: "accessCode", message: "请输入接入码" } : null)) return;
    const button = current.querySelector('[type="submit"]');
    const note = current.querySelector('[role="status"]');
    const request = new AbortController();
    controller = request;
    button.disabled = true;
    button.textContent = "连接中";
    note.hidden = true;
    try {
      await Backend.sipServer.connect({ body: { address, accessCode }, signal: request.signal });
      // 预留接口：完成状态查询和连接生命周期接入后才展示已连接。
      if (form === current && !request.signal.aborted) {
        note.textContent = "SIP 电话服务尚未接入";
        note.hidden = false;
      }
    } catch (error) {
      if (form === current && !request.signal.aborted) {
        note.textContent = error.code === "NOT_CONNECTED" ? "SIP 电话服务尚未接入" : "连接未完成，请稍后重试";
        note.hidden = false;
      }
    } finally {
      codeInput.value = "";
      if (controller === request) controller = null;
      if (form === current) { button.disabled = false; button.textContent = "连接"; }
    }
  }
  function unmount() {
    controller?.abort(); controller = null;
    form?.removeEventListener("submit", connect);
    form?.reset(); form = null;
  }
  function mount() {
    unmount();
    form = document.getElementById("sip-server-form");
    form?.addEventListener("submit", connect);
  }
  return { render, mount, unmount };
})();
