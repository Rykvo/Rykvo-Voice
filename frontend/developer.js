const Developer = (() => {
  const groups = [
    {
      title: "API",
      fields: [
        {
          name: "username",
          label: "Username",
          placeholder: "Username",
          autocomplete: "username",
          maxLength: 64,
        },
        {
          name: "apiKey",
          label: "API Key",
          type: "password",
          placeholder: "API Key",
          maxLength: 512,
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
  function render() {
    return `<section class="form-page developer-page">${Forms.header("开发者", "developer.svg")}<form id="developer-form" class="settings-form" novalidate>${groups.map((group) => `<section class="form-section"><h2>${group.title}</h2><div class="form-fields">${group.fields.map((field) => Forms.field({ prefix: "developer", ...field })).join("")}</div></section>`).join("")}<div class="form-footer"><button class="primary" type="submit">保存修改</button></div></form></section>`;
  }
  function validate(values) {
    if (values.webhook.trim()) {
      try {
        const url = new URL(values.webhook.trim());
        if (
          !["http:", "https:"].includes(url.protocol) ||
          url.username ||
          url.password
        )
          throw Error();
      } catch {
        return { field: "webhook", message: "请输入有效的 HTTP 或 HTTPS 地址" };
      }
    }
    if (values.adminId.trim() && !/^[1-9]\d{0,18}$/.test(values.adminId.trim()))
      return { field: "adminId", message: "管理员 ID 请填写正整数" };
    if (
      values.notificationId.trim() &&
      !/^-?[1-9]\d{0,18}$/.test(values.notificationId.trim())
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
  document.addEventListener("submit", (event) => {
    if (event.target.id !== "developer-form") return;
    event.preventDefault();
    const values = Object.fromEntries(new FormData(event.target));
    if (Forms.report(event.target, validate(values))) return;
    // 密钥由后端保管，前端不缓存或发送至第三方。
    UI.toast("配置服务尚未连接，修改未保存");
  });
  return { render, validate };
})();
