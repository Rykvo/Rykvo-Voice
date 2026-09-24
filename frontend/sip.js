const SIP = (() => {
  const { escape: esc, $ } = UI;
  // 尚未接入后端，凭据只保留在当前页面内存中。
  let records = [],
    query = "",
    editing = null,
    historyDraft = null;
  function serverIP() {
    const host = location.hostname.replace(/^\[|\]$/g, "");
    return /^(?:\d{1,3}\.){3}\d{1,3}$/.test(host) || host.includes(":")
      ? host
      : "—";
  }
  function validate(value, id = "") {
    if (
      !value.username.trim() ||
      value.username.trim().length > 128 ||
      /[\x00-\x1f\x7f]/.test(value.username)
    )
      return { field: "username", message: "请输入有效用户名" };
    if (
      !value.password ||
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
    if (!/^\d{1,5}$/.test(port) || Number(port) < 1 || Number(port) > 65535)
      return { field: "port", message: "端口范围为 1–65535" };
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
    if (record.ip === "—") return "—";
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
              `<tr><th scope="row">${esc(endpoint(item)).replace(/:(\d+)$/, "<wbr>:$1")}</th><td>${esc(item.username)}</td><td><span class="sip-password" aria-label="密码已隐藏">••••••</span></td><td>${item.allocation === "fixed" ? "固定" : "全部"}</td><td><button type="button" class="text-button sip-detail" data-sip-detail="${esc(item.id)}" aria-label="查看 ${esc(item.username)} 详情">详情</button></td></tr>`,
          )
          .join("")
      : `<tr><td colspan="5" class="data-empty">${term ? "无匹配结果" : "暂无 SIP 账号"}</td></tr>`;
  }
  function updateRows() {
    const body = $("#sip-rows");
    if (body) body.innerHTML = rows();
  }
  function render() {
    query = "";
    return `<section class="sip-page"><button type="button" class="text-button form-back" data-action="general-back">‹ 通用</button><div class="page-head"><h1>SIP 电话</h1></div><div class="data-toolbar"><label class="data-search"><svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="8.5" cy="8.5" r="5.5"/><path d="m13 13 4 4"/></svg><input id="sip-search" type="search" aria-label="搜索用户名" placeholder="搜索用户名" autocomplete="off"></label><button type="button" class="primary" data-sip-create>创建</button></div><div class="data-table-wrap" role="region" aria-label="SIP 列表滚动区域" tabindex="0"><table class="data-table sip-table" aria-label="SIP 电话列表"><colgroup><col class="sip-col-ip"><col class="sip-col-username"><col class="sip-col-password"><col class="sip-col-modules"><col class="sip-col-actions"></colgroup><thead><tr><th scope="col">IP</th><th scope="col">用户名</th><th scope="col">密码</th><th scope="col">模块</th><th scope="col">操作</th></tr></thead><tbody id="sip-rows">${rows()}</tbody></table></div></section>`;
  }
  function allocationHTML(record) {
    const fixed = record.allocation === "fixed";
    return `<section class="sip-allocation"><h3>分配模块</h3><div class="sip-segment" role="radiogroup" aria-label="分配模块"><label><input type="radio" name="allocation" value="all" ${fixed ? "" : "checked"}><span>全部</span></label><label><input type="radio" name="allocation" value="fixed" ${fixed ? "checked" : ""}><span>固定</span></label></div><fieldset id="sip-fixed-modules" ${fixed ? "" : "hidden disabled"}><legend class="sr-only">固定模块</legend><div id="sip-modules" class="sip-module-list" role="group" aria-label="固定模块" tabindex="-1">${ModuleData.items.map((item) => `<label><input type="checkbox" name="moduleIds" value="${esc(item.id)}" ${(record.moduleIds || []).includes(item.id) ? "checked" : ""}><span><strong>${esc(item.label || item.name)}</strong><small>${esc(Countries.format(item.number))}</small></span></label>`).join("") || '<p class="data-empty">暂无模块</p>'}</div></fieldset></section>`;
  }
  const historyHTML = SIPHistory.render;
  function open(record = null) {
    editing = record?.id || "";
    const fields = [
      { name: "username", label: "用户名", placeholder: "输入用户名" },
      {
        name: "password",
        label: "密码",
        type: "password",
        placeholder: "输入密码",
        autocomplete: "new-password",
      },
      {
        name: "port",
        label: "端口",
        placeholder: "5060",
        inputmode: "numeric",
        maxLength: 5,
      },
    ];
    UI.modal(
      record ? "编辑 SIP 账号" : "创建 SIP 账号",
      `<form id="sip-form" class="settings-form" data-sip-editor="${record ? "edit" : "create"}" novalidate><div class="form-fields">${fields.map((field) => Forms.field({ ...field, prefix: "sip", required: true, value: record?.[field.name] ?? (field.name === "port" ? "5060" : undefined) })).join("")}</div>${record ? allocationHTML(record) + `<label class="sip-receive"><span>接电话</span><span class="form-switch"><input type="checkbox" role="switch" name="receiveCalls" aria-label="接电话" ${record.receiveCalls ? "checked" : ""}><span aria-hidden="true"></span></span></label>` + '<button type="button" class="sip-history-button" data-sip-history><span>通话记录</span><span aria-hidden="true">›</span></button>' : ""}<div class="sip-form-actions"><button type="button" class="text-button" data-sip-cancel>取消</button><button type="submit" class="primary">${record ? "保存" : "创建"}</button></div>${record ? `<button type="button" class="text-button sip-delete" data-sip-delete="${esc(record.id)}">删除账号</button>` : ""}</form>`,
    );
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
      historyDraft = { ...record, ...formValues($("#sip-form")) };
      UI.modal(
        "通话记录",
        `<section class="sip-history-window"><button type="button" class="text-button form-back" data-sip-history-back>‹ 编辑 SIP 账号</button>${historyHTML(record.id)}</section>`,
      );
      $("[data-sip-history-back]").focus();
      return;
    }
    if (event.target.closest("[data-sip-history-back]")) {
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
    if (record)
      ContextMenu.confirm("删除 SIP 账号？", () => {
        records = records.filter((item) => item.id !== record.id);
        updateRows();
        UI.toast("已删除");
      });
  });
  document.addEventListener("submit", (event) => {
    if (event.target.id !== "sip-form") return;
    event.preventDefault();
    const form = event.target;
    const value = formValues(form);
    if (value.allocation !== "fixed") value.moduleIds = [];
    const error = validate(value, editing);
    if (error?.field === "moduleIds" || error?.field === "allocation") {
      UI.toast(error.message);
      $(
        error.field === "moduleIds" ? "#sip-modules" : ".sip-segment input",
      ).focus();
      return;
    }
    if (Forms.report(form, error)) return;
    const record = {
      id: editing || UI.id(),
      ip: serverIP(),
      port: Number(value.port.trim()),
      username: value.username.trim(),
      password: value.password,
      receiveCalls: value.receiveCalls,
      allocation: value.allocation,
      moduleIds: [...new Set(value.moduleIds)],
    };
    if (editing)
      records = records.map((item) => (item.id === editing ? record : item));
    else {
      records.push(record);
      query = "";
      const input = $("#sip-search");
      if (input) input.value = "";
    }
    $("#dialog").close();
    updateRows();
    UI.toast("已保存到当前预览");
  });
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
  $("#dialog").addEventListener("close", () => {
    if ($("#dialog").open) return;
    const form = $("#sip-form");
    if (form) {
      form.reset();
      form.remove();
    }
    editing = null;
    historyDraft = null;
  });
  return { render, validate, historyHTML };
})();
