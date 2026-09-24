// 只控制显示；授权与密码均由服务端验证。
const Visibility = (() => {
  const key = "rykvo-voice-visibility-v1";
  const navigation = [
    { id: "modules", title: "模块" },
    { id: "phone", title: "电话" },
    { id: "messages", title: "信息" },
    { id: "general", title: "通用" },
  ];
  let settings = [],
    state = {},
    currentPage = () => "",
    initialized = false;
  let protectedDialog = false,
    verified = false,
    busy = false,
    generation = 0;
  let hasServerPreferences = false;
  let gestureActive = false,
    gestureGeneration = 0,
    activation = Promise.resolve();
  function accept(features) {
    if (!features || typeof features !== "object" || Array.isArray(features))
      return;
    state = Object.fromEntries(
      [...navigation, ...settings].map((item) => [
        item.id,
        features[item.id] !== false,
      ]),
    );
    apply();
  }
  async function sync() {
    try {
      const data = await Backend.visibility.get();
      if (data?.features) {
        hasServerPreferences = true;
        accept(data.features);
        UI.write(key, state);
      }
    } catch {
      /* 离线保留显示状态，不授予编辑权限。 */
    }
  }
  function visible(id) {
    return (
      state[id] !== false &&
      (!settings.some((item) => item.id === id) || state.general !== false)
    );
  }
  function apply() {
    document.querySelectorAll(".nav-item[data-page]").forEach((button) => {
      button.hidden = !visible(button.dataset.page);
    });
    document
      .querySelectorAll(".general-entry[data-action]")
      .forEach((button) => {
        button.hidden = !visible(button.dataset.action);
      });
    const divider = UI.$(".nav-divider");
    if (divider)
      divider.hidden =
        !visible("general") ||
        !navigation.some((item) => item.id !== "general" && visible(item.id));
    const list = UI.$(".general-list");
    if (list) list.hidden = !settings.some((item) => visible(item.id));
    const main = UI.$("#app-window main");
    if (main) main.hidden = !visible(currentPage());
    document.querySelectorAll("[data-feature-visible]").forEach((input) => {
      input.checked = state[input.dataset.featureVisible];
    });
  }
  function field(name, label, autocomplete) {
    return Forms.field({
      prefix: "visibility",
      name,
      label,
      type: "password",
      autocomplete,
      placeholder: name === "password" ? "输入密码" : "",
      required: true,
    });
  }
  function group(title, items) {
    return `<section class="visibility-group"><h3>${title}</h3><div>${items.map((item) => `<div class="visibility-row"><span>${UI.escape(item.title)}</span><label class="form-switch"><input type="checkbox" role="switch" data-feature-visible="${UI.escape(item.id)}" aria-label="显示${UI.escape(item.title)}" ${state[item.id] ? "checked" : ""}><span aria-hidden="true"></span></label></div>`).join("")}</div></section>`;
  }
  function passwordPrompt() {
    verified = false;
    busy = false;
    protectedDialog = true;
    generation++;
    UI.modal(
      "验证密码",
      `<form id="visibility-unlock" class="visibility-verify" novalidate><div class="form-fields">${field("password", "密码", "off")}</div><p class="visibility-error" role="status" aria-live="polite"></p><button type="submit" class="primary">继续</button></form>`,
    );
    UI.$("#visibility-password")?.focus();
  }
  function open() {
    UI.modal(
      "功能显示",
      `<div class="visibility-settings"><div class="visibility-columns"><div>${group("侧栏", navigation)}<form id="visibility-password-form" class="visibility-password" novalidate><div class="form-fields">${field("currentPassword", "当前密码", "off")}${field("newPassword", "新密码", "new-password")}</div><p class="visibility-error" role="status" aria-live="polite"></p></form></div>${group("通用", settings)}</div><div class="visibility-actions"><button type="submit" form="visibility-password-form" class="visibility-save">保存密码</button><button type="button" class="primary visibility-done" data-visibility-done>完成</button></div></div>`,
    );
  }
  function errorText(error) {
    if (error.code === "INVALID_PASSWORD") return "密码不正确";
    if (error.code === "INVALID_NEW_PASSWORD")
      return "新密码需为 8–128 字节，且与当前密码不同";
    if (error.status === 429) return "尝试过多，请稍后重试";
    if (error.status === 401) return "登录已失效，请重新登录";
    if (error.code === "NOT_CONNECTED" || error.code === "NOT_CONFIGURED")
      return "服务尚未连接";
    return "操作失败，请重试";
  }
  function setBusy(value) {
    busy = value;
    document
      .querySelectorAll(
        ".visibility-settings input, .visibility-settings button, .visibility-verify input, .visibility-verify button",
      )
      .forEach((node) => {
        node.disabled = value;
      });
  }
  function activate(reset = false) {
    if (reset) {
      gestureActive = false;
      gestureGeneration++;
    } else if (!gestureActive) {
      gestureActive = true;
      gestureGeneration++;
    }
    const turn = gestureGeneration;
    activation = activation
      .then(async () => {
        if (!reset && (turn !== gestureGeneration || !gestureActive)) return;
        const data = await Backend.ui.activate({ body: { reset } });
        if (
          !reset &&
          turn === gestureGeneration &&
          gestureActive &&
          data?.challenge === true
        ) {
          gestureActive = false;
          passwordPrompt();
        }
      })
      .catch(() => {
        gestureActive = false;
      });
  }
  async function submit(event) {
    const form = event.target;
    const changing = form.id === "visibility-password-form";
    if (!changing && form.id !== "visibility-unlock") return;
    event.preventDefault();
    if (busy || !protectedDialog || (changing && !verified)) return;
    const status = form.querySelector(".visibility-error");
    const values = Object.fromEntries(new FormData(form));
    const missing = changing
      ? !values.currentPassword || !values.newPassword
      : !values.password;
    if (missing) {
      status.textContent = "请输入密码";
      return;
    }
    if (
      changing &&
      (values.newPassword.length < 8 ||
        values.newPassword === values.currentPassword)
    ) {
      status.textContent = "新密码至少 8 位，且与当前密码不同";
      return;
    }
    const turn = generation;
    status.textContent = "";
    setBusy(true);
    try {
      if (changing) {
        await Backend.visibility.changePassword({ body: values });
        if (turn !== generation) return;
        form.reset();
        UI.$("#dialog").close();
        UI.toast("密码已修改");
      } else {
        const result = await Backend.visibility.unlock({ body: values });
        if (turn !== generation) {
          await Backend.visibility.lock();
          return;
        }
        if (result?.verified !== true) throw new Error("Invalid verification");
        await Backend.visibility.access();
        if (turn !== generation) {
          await Backend.visibility.lock();
          return;
        }
        form.reset();
        verified = true;
        open();
      }
    } catch (error) {
      if (turn !== generation) return;
      if (error.code === "VERIFICATION_REQUIRED") passwordPrompt();
      else status.textContent = errorText(error);
    } finally {
      if (turn === generation) setBusy(false);
    }
  }
  async function change(event) {
    const input = event.target,
      id = input.dataset.featureVisible;
    if (!Object.hasOwn(state, id)) return;
    if (!verified || busy || !protectedDialog) {
      input.checked = state[id];
      return;
    }
    const turn = generation;
    setBusy(true);
    try {
      const result = await Backend.visibility.update({
        body: {
          features: hasServerPreferences
            ? { [id]: input.checked }
            : { ...state, [id]: input.checked },
        },
      });
      if (!result?.features) throw new Error("Invalid preferences");
      accept({ ...state, ...result.features });
      hasServerPreferences = true;
      UI.write(key, state);
    } catch (error) {
      input.checked = state[id];
      if (turn === generation) {
        if (error.code === "VERIFICATION_REQUIRED") passwordPrompt();
        else UI.toast(errorText(error));
      }
    } finally {
      if (turn === generation) setBusy(false);
    }
  }
  function init(items, page) {
    if (initialized) return;
    initialized = true;
    settings = items;
    currentPage = page;
    accept(UI.read(key, {}));
    sync();
    document.addEventListener("click", (event) => {
      if (event.target.closest(".brand-trigger")) {
        if (!protectedDialog) activate();
        return;
      }
      if (gestureActive) activate(true);
      if (event.target.closest("[data-visibility-done]"))
        UI.$("#dialog").close();
    });
    document.addEventListener("submit", submit);
    document.addEventListener("change", change);
    UI.$("#dialog").addEventListener("close", () => {
      if (!protectedDialog) return;
      protectedDialog = false;
      verified = false;
      busy = false;
      generation++;
      UI.$("#dialog-content").replaceChildren();
      Backend.visibility.lock().catch(() => {});
    });
    window.addEventListener("storage", (event) => {
      if (event.key === key) sync();
    });
  }
  return { init, apply, visible };
})();
