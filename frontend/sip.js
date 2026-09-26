const SIP = (() => {
  const { escape: esc, $ } = UI;
  // 列表来自服务端；密码只用于当次提交，不进入账号缓存。
  let records = [],
    query = "",
    editing = null, editingRevision = 0,
    historyDraft = null, active = false, generation = 0, timer = null,
    reading = null, writing = null, loadIssue = "",
    network = null;
  const reservedPorts = [2019, 8080, 51820, 51821, 51822];
  function portHint() {
    if (!network) return reading ? "读取端口范围…" : "端口范围暂不可用";
    if (network.mode === "lan") return "输入端口";
    const ranges = [];
    let start = network.start;
    for (const port of [...reservedPorts, network.end + 1]) {
      if (port < start || port > network.end + 1) continue;
      if (port > start) ranges.push(port === start + 1 ? String(start) : `${start}–${port - 1}`);
      start = port + 1;
    }
    return ranges.join("、") || "暂无可用端口";
  }
  function updatePortHint() {
    const input = $("#sip-port");
    if (input) input.placeholder = input.title = portHint();
  }
  const errors = {
    SIP_ACCOUNT_EXISTS: "该端口下的用户名已存在", SIP_STALE_ACCOUNT: "账号已更新，请重新打开详情",
    SIP_INVALID_USERNAME: "用户名仅支持字母、数字、点、横线和下划线",
    SIP_PASSWORD_REQUIRED: "修改用户名时请重新输入密码", SIP_INVALID_PASSWORD: "请输入有效密码",
    SIP_INVALID_PORT: "端口不在可用范围内", SIP_PORT_IN_USE: "端口已占用或暂不可用", SIP_INVALID_MODULES: "请选择有效模块",
    SIP_NETWORK_UNAVAILABLE: "服务器配置暂不可用", SIP_ACCOUNT_LIMIT: "账号数量已达上限",
    NOT_CONNECTED: "服务尚未连接",
  };
  function statusHTML(value) {
    const status = ["online", "busy", "offline"].includes(value) ? value : "unknown";
    return `<span class="sip-status" data-status="${status}">${({ online: "在线", busy: "通话中", offline: "离线", unknown: "—" })[status]}</span>`;
  }
  function validate(value, id = "") {
    if (
      !/^[A-Za-z0-9_.-]{1,64}$/.test(value.username.trim())
    )
      return { field: "username", message: "请输入有效用户名" };
    if (
      (!id && !value.password) ||
      value.password.length > 128 ||
      /[\x00-\x1f\x7f]/.test(value.password)
    )
      return { field: "password", message: "请输入有效密码" };
    if (value.allocation && !["all", "fixed"].includes(value.allocation))
      return { field: "allocation", message: "请选择分配方式" };
    if (
      value.allocation === "fixed" &&
      (!Array.isArray(value.moduleIds) ||
        !value.moduleIds.length ||
        value.moduleIds.some(
          (id) => !ModuleData.items.some((item) => item.id === id),
        ))
    )
      return { field: "moduleIds", message: "请至少选择一个固定模块" };
    const port = value.port.trim();
    if (!network) return { field: "port", message: "端口范围暂不可用" };
    if (!/^\d{1,5}$/.test(port) || Number(port) < network.start || Number(port) > network.end || reservedPorts.includes(Number(port)))
      return { field: "port", message: network.mode === "lan" ? "请输入 1024–65535 内未被占用的非保留端口" : `可用端口 ${portHint()}` };
    if (
      records.some(
        (item) =>
          item.id !== id &&
          item.port === Number(port) &&
          item.username === value.username.trim(),
      )
    )
      return { field: "username", message: "该端口下的用户名已存在" };
    return null;
  }
  function endpoint(record) {
    if (!record.ip || record.ip === "—") return "—";
    return `${record.ip.includes(":") ? `[${record.ip}]` : record.ip}:${record.port}`;
  }
  function rows() {
    const term = query.trim().toLowerCase();
    const visible = records.filter((item) =>
      item.username.toLowerCase().includes(term),
    );
    return visible.length
      ? visible
          .map(
            (item) =>
              `<tr><th scope="row">${esc(endpoint(item)).replace(/:(\d+)$/, "<wbr>:$1")}</th><td>${esc(item.username)}</td><td><span class="sip-password" aria-label="密码已隐藏">••••••</span></td><td>${item.allocation === "fixed" ? "固定" : "全部"}</td><td>${statusHTML(item.status)}</td><td><button type="button" class="text-button sip-detail" data-sip-detail="${esc(item.id)}" aria-label="查看 ${esc(item.username)} 详情">详情</button></td></tr>`,
          )
          .join("")
      : `<tr><td colspan="6" class="data-empty">${esc(loadIssue || (reading ? "加载中" : term ? "无匹配结果" : "暂无 SIP 账号"))}</td></tr>`;
  }
  function updateRows() {
    const body = $("#sip-rows");
    if (body) { const html = rows(); if (body.innerHTML !== html) body.innerHTML = html; }
    const note = $("#sip-service-status");
    if (note) note.textContent = loadIssue;
  }
  function render() {
    query = "";
    return `<section class="sip-page"><button type="button" class="text-button form-back" data-action="general-back">‹ 通用</button><div class="page-head"><h1>SIP 电话</h1></div><div class="data-toolbar"><label class="data-search"><svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="8.5" cy="8.5" r="5.5"/><path d="m13 13 4 4"/></svg><input id="sip-search" type="search" aria-label="搜索用户名" placeholder="搜索用户名" autocomplete="off"></label><button type="button" class="primary" data-sip-create>创建</button></div><div class="data-table-wrap" role="region" aria-label="SIP 列表滚动区域" tabindex="0"><table class="data-table sip-table" aria-label="SIP 电话列表"><colgroup><col class="sip-col-ip"><col class="sip-col-username"><col class="sip-col-password"><col class="sip-col-modules"><col class="sip-col-status"><col class="sip-col-actions"></colgroup><thead><tr><th scope="col">IP</th><th scope="col">用户名</th><th scope="col">密码</th><th scope="col">模块</th><th scope="col">状态</th><th scope="col">操作</th></tr></thead><tbody id="sip-rows">${rows()}</tbody></table></div><p id="sip-service-status" class="sip-service-status" role="status"></p></section>`;
  }
  function allocationHTML(record) {
    const fixed = record.allocation === "fixed";
    return `<section class="sip-allocation"><h3>分配模块</h3><div class="sip-segment" role="radiogroup" aria-label="分配模块"><label><input type="radio" name="allocation" value="all" ${fixed ? "" : "checked"}><span>全部</span></label><label><input type="radio" name="allocation" value="fixed" ${fixed ? "checked" : ""}><span>固定</span></label></div><fieldset id="sip-fixed-modules" ${fixed ? "" : "hidden disabled"}><legend class="sr-only">固定模块</legend><div id="sip-modules" class="sip-module-list" role="group" aria-label="固定模块" tabindex="-1">${ModuleData.items.map((item) => `<label><input type="checkbox" name="moduleIds" value="${esc(item.id)}" ${(record.moduleIds || []).includes(item.id) ? "checked" : ""}><span><strong>${esc(item.label || item.name)}</strong><small>${esc(Countries.format(item.number))}</small></span></label>`).join("") || '<p class="data-empty">暂无模块</p>'}</div></fieldset></section>`;
  }
  const historyHTML = SIPHistory.render;
  function open(record = null) {
    if (writing) return;
    editing = record?.id || "";
    editingRevision = record?.revision || 0;
    const fields = [
      { name: "username", label: "用户名", placeholder: "输入用户名" },
      {
        name: "password",
        label: "密码",
        type: "password",
        placeholder: record ? "留空保持不变" : "输入密码",
        autocomplete: "new-password",
      },
      {
        name: "port",
        label: "端口",
        placeholder: portHint(),
        inputmode: "numeric",
        maxLength: 5,
      },
    ];
    UI.modal(
      record ? "编辑 SIP 账号" : "创建 SIP 账号",
      `<form id="sip-form" class="settings-form" data-sip-editor="${record ? "edit" : "create"}" novalidate><div class="form-fields">${fields.map((field) => Forms.field({ ...field, prefix: "sip", required: field.name !== "password" || !record, value: field.name === "password" ? undefined : record?.[field.name] })).join("")}</div>${record ? allocationHTML(record) + `<label class="sip-receive"><span>接电话</span><span class="form-switch"><input type="checkbox" role="switch" name="receiveCalls" aria-label="接电话" ${record.receiveCalls ? "checked" : ""}><span aria-hidden="true"></span></span></label>` + '<button type="button" class="sip-history-button" data-sip-history><span>通话记录</span><span aria-hidden="true">›</span></button>' : ""}<div class="sip-form-actions"><button type="button" class="text-button" data-sip-cancel>取消</button><button type="submit" class="primary">${record ? "保存" : "创建"}</button></div>${record ? `<button type="button" class="text-button sip-delete" data-sip-delete="${esc(record.id)}">删除账号</button>` : ""}</form>`,
    );
    updatePortHint();
    $("#sip-username").focus();
  }
  function formValues(form) {
    return {
      ...Object.fromEntries(
        ["username", "password", "port"].map((name) => [
          name,
          form.elements.namedItem(name).value,
        ]),
      ),
      receiveCalls: editing
        ? !!form.elements.namedItem("receiveCalls")?.checked
        : false,
      allocation: editing ? form.elements.namedItem("allocation").value : "all",
      moduleIds: [...form.querySelectorAll('[name="moduleIds"]:checked')].map(
        (input) => input.value,
      ),
    };
  }
  document.addEventListener("click", (event) => {
    if (event.target.closest("[data-sip-create]")) return open();
    if (event.target.closest("[data-sip-cancel]")) return $("#dialog").close();
    if (event.target.closest("[data-sip-history]")) {
      const record = records.find((item) => item.id === editing);
      if (!record) return;
      historyDraft = { ...record, ...formValues($("#sip-form")), revision: editingRevision };
      delete historyDraft.password;
      $("#sip-form").elements.namedItem("password").value = "";
      UI.modal(
        "通话记录",
        `<section class="sip-history-window"><button type="button" class="text-button form-back" data-sip-history-back>‹ 编辑 SIP 账号</button>${historyHTML(record.id)}</section>`,
      );
      SIPHistory.update();
      $("[data-sip-history-back]").focus();
      return;
    }
    if (event.target.closest("[data-sip-history-back]")) {
      SIPHistory.close();
      if (historyDraft) open(historyDraft);
      historyDraft = null;
      return;
    }
    const edit = event.target.closest("[data-sip-detail]");
    if (edit) {
      const record = records.find((item) => item.id === edit.dataset.sipDetail);
      if (record) open(record);
      return;
    }
    const remove = event.target.closest("[data-sip-delete]");
    const record =
      remove && records.find((item) => item.id === remove.dataset.sipDelete);
    if (record) {
      const revision = editing === record.id ? editingRevision : record.revision;
      ContextMenu.confirm("删除 SIP 账号并挂断相关通话？", () => mutate("remove", record.id, { revision }));
    }
  });
  document.addEventListener("submit", async (event) => {
    if (event.target.id !== "sip-form") return;
    event.preventDefault();
    if (writing) return;
    const form = event.target, value = formValues(form);
    if (value.allocation !== "fixed") value.moduleIds = [];
    const error = validate(value, editing);
    if (error?.field === "moduleIds" || error?.field === "allocation") {
      UI.toast(error.message);
      $(error.field === "moduleIds" ? "#sip-modules" : ".sip-segment input").focus();
      return;
    }
    if (Forms.report(form, error)) return;
    const body = { ...value, username: value.username.trim(), port: Number(value.port), revision: editingRevision };
    form.elements.namedItem("password").value = "";
    await mutate(editing ? "update" : "create", editing, body);
  });
  async function mutate(operation, id, body) {
    if (!active || writing) return;
    const epoch = generation, request = new AbortController();
    writing = request; clearTimeout(timer); reading?.abort();
    const form = $("#sip-form");
    form?.querySelectorAll("button,input").forEach((node) => { node.disabled = true; });
    try {
      await Backend.sip[operation]({ params: { accountId: id }, body, signal: request.signal });
      if (active && epoch === generation) {
        if ($("#sip-form") === form) $("#dialog").close();
        UI.toast(operation === "remove" ? "已删除" : "已保存");
      }
    } catch (error) {
      if (active && epoch === generation && !request.signal.aborted) UI.toast(errors[error.code] || error.message);
    } finally {
      delete body.password;
      if (writing === request) writing = null;
      if (active && epoch === generation) {
        form?.querySelectorAll("button,input").forEach((node) => { node.disabled = false; });
        await load();
      }
    }
  }
  async function load() {
    clearTimeout(timer);
    if (!active || writing || document.hidden) return;
    reading?.abort();
    const request = new AbortController(), epoch = generation;
    reading = request;
    try {
      const data = await Backend.sip.list({ signal: request.signal });
      if (!active || epoch !== generation || request.signal.aborted) return;
      records = data.items.map(({ password, ...item }) => item);
      const candidate = data.network;
      network = data.networkReady !== false && Number.isInteger(candidate?.start) && Number.isInteger(candidate?.end) && candidate.start >= 1024 && candidate.end <= 65535 && candidate.start <= candidate.end ? candidate : null;
      loadIssue = "";
    } catch (error) {
      if (!active || epoch !== generation || request.signal.aborted) return;
      network = null;
      loadIssue = errors[error.code] || "加载失败，请重试";
      records = records.map((item) => ({ ...item, status: null }));
    } finally {
      if (reading === request) reading = null;
      if (active && epoch === generation && !request.signal.aborted) {
        updateRows();
        updatePortHint();
        SIPHistory.update();
        timer = setTimeout(load, 5000);
      }
    }
  }

  document.addEventListener("change", (event) => {
    if (
      event.target.name !== "allocation" ||
      !event.target.closest("#sip-form")
    )
      return;
    const fixed = event.target.value === "fixed";
    $("#sip-fixed-modules").hidden = !fixed;
    $("#sip-fixed-modules").disabled = !fixed;
    if (fixed) $("#sip-modules").focus();
  });
  function search(event) {
    if (
      event.target.id !== "sip-search" ||
      event.isComposing ||
      query === event.target.value
    )
      return;
    query = event.target.value;
    updateRows();
  }
  document.addEventListener("input", search);
  document.addEventListener("compositionend", search);
  document.addEventListener("visibilitychange", () => {
    if (!active) return;
    if (document.hidden) { clearTimeout(timer); reading?.abort(); }
    else if (!writing) load();
  });
  $("#dialog").addEventListener("close", () => {
    if ($("#dialog").open) return;
    SIPHistory.close();
    const form = $("#sip-form");
    if (form) {
      form.reset();
      form.remove();
    }
    editing = null;
    historyDraft = null;
  });
  function unmount() {
    SIPHistory.close();
    active = false; generation++; clearTimeout(timer);
    reading?.abort(); writing?.abort(); reading = writing = null;
    network = null; records = []; historyDraft = null; editing = null;
    const form = $("#sip-form");
    if (form) { form.reset(); $("#dialog").close(); }
  }
  function mount() { unmount(); active = true; return load(); }
  return { render, validate, historyHTML, mount, unmount };
})();
