const Developer = (() => {
  const groups = [
    {
      title: "API",
      fields: [
        {
          name: "apiKey",
          label: "API Key",
          type: "password",
          placeholder: "API Key",
          maxLength: 256,
        },
      ],
    },
    {
      title: "短信 Webhook",
      fields: [
        {
          name: "webhook",
          label: "地址",
          type: "url",
          placeholder: "https://",
          maxLength: 2048,
        },
      ],
    },
    {
      title: "Telegram 机器人",
      fields: [
        {
          name: "botToken",
          label: "机器人令牌",
          type: "password",
          placeholder: "机器人令牌",
          maxLength: 256,
        },
        {
          name: "adminId",
          label: "管理员 ID",
          placeholder: "管理员 ID",
          maxLength: 20,
        },
        {
          name: "notificationId",
          label: "通知 ID",
          placeholder: "用户、群组或频道 ID",
          maxLength: 20,
        },
        {
          name: "telegramProxy",
          label: "socks5",
          type: "password",
          placeholder: "IP:端口:账号:密码",
          maxLength: 2048,
        },
      ],
    },
  ];
  const secrets = [
    ["apiKey", "hasApiKey", "API Key", "清除 API Key"],
    ["botToken", "hasBotToken", "机器人令牌", "清除令牌"],
    ["telegramProxy", "hasTelegramProxy", "IP:端口:账号:密码", "清除代理"],
  ];
  let snapshot = null, request = null, draft = null;
  function section(group) {
    const fields = group.fields.map(field => Forms.field({ prefix: "developer", ...field })).join("");
    const clear = secrets.filter(([name]) => group.fields.some(field => field.name === name))
      .map(([name, , , label]) => `<button type="button" class="text-button" data-clear-secret="${name}" hidden>${label}</button>`).join("");
    return `<section class="form-section"><h2>${group.title}</h2><fieldset class="form-fields" disabled>${fields}</fieldset>${clear ? `<div class="developer-secret-actions">${clear}</div>` : ""}</section>`;
  }
  function render() {
    const actions = '<button type="button" class="text-button" data-action="developerDocs">开发者</button>';
    return `<section class="form-page developer-page">${Forms.header("开发者", "developer.svg", actions)}<form id="developer-form" class="settings-form" novalidate>${groups.map(section).join("")}<p id="telegram-notice" class="field-error" role="status" hidden></p><div class="form-footer"><button class="primary" type="submit" disabled>保存</button></div></form></section>`;
  }
  function validate(values) {
    const key = values.apiKey?.trim();
    if (key && (key.length < 6 || key.length > 256 || /[\s\x00-\x1f\x7f]/.test(key)))
      return { field: "apiKey", message: "密钥需为 6–256 位，且不含空格" };
    if ((values.webhook || "").trim()) {
      try {
        const url = new URL((values.webhook || "").trim());
        if (
          url.protocol !== "https:" || (url.port && url.port !== "443") ||
          url.username || url.password || url.hash
        )
          throw Error();
      } catch {
        return { field: "webhook", message: "请输入有效的 HTTPS 地址" };
      }
    }
    if ((values.adminId || "").trim() && !/^[1-9]\d{0,18}$/.test((values.adminId || "").trim()))
      return { field: "adminId", message: "管理员 ID 请填写正整数" };
    if (
      (values.notificationId || "").trim() &&
      !/^-?[1-9]\d{0,18}$/.test((values.notificationId || "").trim())
    )
      return {
        field: "notificationId",
        message: "通知 ID 请填写用户、群组或频道的数字 ID",
      };
    if (values.telegramProxy?.trim()) {
      const parts = values.telegramProxy.match(
        /^(\[[^\]]+\]|[^:]+):(\d+):([^:]*):(.*)$/,
      );
      let validIP = false;
      if (parts) {
        const host = parts[1];
        if (/^\d{1,3}(\.\d{1,3}){3}$/.test(host)) {
          validIP = host.split(".").every((part) => Number(part) <= 255);
        } else if (host.startsWith("[") && host.includes(":")) {
          try {
            validIP = new URL(`http://${host}`).hostname.startsWith("[");
          } catch {}
        }
      }
      if (
        !parts ||
        !validIP ||
        Number(parts[2]) < 1 ||
        Number(parts[2]) > 65535 ||
        Boolean(parts[3]) !== Boolean(parts[4]) ||
        /[\r\n\x00]/.test(values.telegramProxy)
      ) {
        return {
          field: "telegramProxy",
          message: "格式：IP:端口:账号:密码，端口范围 1–65535",
        };
      }
    }
    return null;
  }
  function setBusy(form, busy) {
    form.querySelectorAll("fieldset").forEach(fieldset => { fieldset.disabled = busy; });
    form.querySelector('[type="submit"]').disabled = busy;
    form.closest(".developer-page").querySelector('[data-action="developerDocs"]').disabled = busy;
  }
  function apply(form, data) {
    snapshot = data;
    form.dataset.revision = data.revision;
    form.dataset.apiRevision = data.apiRevision;
    for (const name of ["webhook", "adminId", "notificationId"]) form.elements.namedItem(name).value = data[name] || "";
    for (const [name, flag, placeholder] of secrets) {
      const input = form.elements.namedItem(name);
      input.value = ""; input.type = "password"; delete input.dataset.clear;
      input.placeholder = data[flag] ? "已设置，留空保持不变" : placeholder;
      form.querySelector(`[data-clear-secret="${name}"]`).hidden = !data[flag];
      form.querySelector(`[data-secret-toggle="developer-${name}"]`)?.setAttribute("aria-pressed", "false");
    }
    const notice = form.querySelector("#telegram-notice");
    notice.hidden = !data.deliveryIssue;
    const issues = { INVALID_BOT_TOKEN: "机器人令牌无效", TELEGRAM_ACCESS_DENIED: "机器人没有发送权限", TELEGRAM_DESTINATION_INVALID: "通知 ID 无效", TELEGRAM_CONNECTION_FAILED: "通知连接失败", TELEGRAM_RATE_LIMIT: "通知发送受限", DELIVERY_UNCONFIRMED: "最近一次通知结果待确认" };
    notice.textContent = issues[data.deliveryIssue] || "通知发送失败";
  }
  async function mount() {
    request?.abort(); request = null;
    if (typeof Backend === "undefined" || !Backend.enabled("developer")) return;
    const form = document.getElementById("developer-form");
    if (draft) {
      apply(form, draft.snapshot);
      for (const [name, value] of Object.entries(draft.values)) {
        const input = form.elements.namedItem(name);
        input.value = value.value;
        if (value.clear) { input.dataset.clear = "true"; input.placeholder = "保存后清除"; }
      }
      draft = null; setBusy(form, false); return;
    }
    const controller = request = new AbortController();
    try {
      const data = await Backend.developer.get({ signal: controller.signal });
      if (request !== controller || controller.signal.aborted || !form.isConnected) return;
      apply(form, data); setBusy(form, false);
    } catch (error) {
      if (!controller.signal.aborted) UI.toast("配置读取失败，请重新打开");
    }
  }
  function unmount(next) {
    const form = document.getElementById("developer-form");
    if (next === "developerDocs" && snapshot && form) {
      draft = { snapshot, values: Object.fromEntries(groups.flatMap(group => group.fields.map(({ name }) => {
        const input = form.elements.namedItem(name);
        return [name, { value: input.value, clear: input.dataset.clear === "true" }];
      }))) };
    } else draft = null;
    request?.abort(); request = null; snapshot = null;
  }
  function discardDraft() { draft = null; }
  function payload(values, form) {
    const body = { revision: Number(form.dataset.revision), apiRevision: Number(form.dataset.apiRevision),
      webhook: values.webhook?.trim() || "",
      adminId: values.adminId?.trim() || "", notificationId: values.notificationId?.trim() || "" };
    for (const [name] of secrets) {
      if (values[name]?.trim()) body[name] = values[name].trim();
      else if (form.elements.namedItem(name).dataset.clear === "true") body[name] = null;
    }
    return body;
  }
  document.addEventListener("click", event => {
    const button = event.target.closest("[data-clear-secret]");
    if (!button) return;
    const form = document.getElementById("developer-form");
    if (!form || !snapshot) return;
    const input = form.elements.namedItem(button.dataset.clearSecret);
    if (!input) return;
    input.value = ""; input.dataset.clear = "true"; input.placeholder = "保存后清除"; input.focus();
  });
  document.addEventListener("submit", async event => {
    const form = event.target;
    if (form.id !== "developer-form") return;
    event.preventDefault();
    if (typeof Backend === "undefined" || !Backend.enabled("developer")) { UI.toast("配置服务尚未连接，修改未保存"); return; }
    const values = Object.fromEntries(new FormData(form));
    if (Forms.report(form, validate(values))) return;
    if (!snapshot || form.querySelector('[type="submit"]').disabled) return;
    const body = payload(values, form);
    setBusy(form, true);
    try {
      const data = await Backend.developer.update({ body });
      if (form.isConnected) apply(form, data);
      UI.toast("已保存");
    } catch (error) {
      const errors = {
        INVALID_BOT_TOKEN: ["botToken", "机器人令牌格式不支持"], INVALID_TELEGRAM_PROXY: ["telegramProxy", "代理格式不支持，请重新填写"],
        INVALID_TELEGRAM_ID: ["notificationId", "请检查通知 ID"],
        INVALID_API_KEY: ["apiKey", "API Key 格式不支持"], INVALID_WEBHOOK_URL: ["webhook", "请输入公开的 HTTPS 地址"],
        WEBHOOK_API_KEY_REQUIRED: ["apiKey", "请先填写 API Key"],
      };
      if (form.isConnected && errors[error.code]) Forms.report(form, { field: errors[error.code][0], message: errors[error.code][1] });
      else UI.toast(error.code === "SETTINGS_CHANGED" ? "配置已更新，请重新打开" : "保存失败，请重试");
    } finally { if (form.isConnected) setBusy(form, false); }
  });
  return { render, validate, mount, unmount, payload, discardDraft };
})();
