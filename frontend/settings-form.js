// Shared lifecycle for revisioned host settings; writes are never retried automatically.
const SettingsForm = {
  create({ selector, fields, api, issueMessage = "" }) {
    let controller = null;
    function unmount() {
      controller?.abort();
      controller = null;
    }
    async function mount() {
      unmount();
      const form = UI.$(selector);
      if (!form) return;
      const request = controller = new AbortController();
      const controls = fields.map(field => ({ ...field, input: form.elements.namedItem(field.name) }));
      const button = form.querySelector('[type="submit"]');
      const error = form.querySelector("[data-settings-error]");
      const retry = form.querySelector("[data-settings-retry]");
      controls.forEach(({ input }) => input.setAttribute("aria-describedby", error.id));
      let revision = 0, busy = false;
      const active = () => controller === request && !request.signal.aborted && form.isConnected;
      const showError = text => {
        error.textContent = text;
        error.hidden = !text;
        retry.hidden = !text;
      };
      const loading = value => {
        busy = value;
        controls.forEach(({ input }) => { input.disabled = value || !revision; });
        button.disabled = value || !revision;
        retry.disabled = value;
      };
      const accept = result => {
        if (controls.some(({ name, valid }) => !valid(result?.[name])) || !Number.isSafeInteger(result?.revision) || result.revision < 1) throw Error("INVALID_RESPONSE");
        controls.forEach(({ name, input }) => { input.value = result[name]; });
        revision = result.revision;
        if (result.issue && issueMessage) showError(issueMessage);
      };
      async function load() {
        if (busy || !active()) return;
        revision = 0;
        loading(true);
        showError("");
        try {
          const result = await api.get({ signal: request.signal });
          if (active()) accept(result);
        } catch {
          if (active()) showError("读取失败，请重试");
        } finally {
          if (active()) loading(false);
        }
      }
      retry.addEventListener("click", load, { signal: request.signal });
      form.addEventListener("input", () => {
        controls.forEach(({ input }) => input.removeAttribute("aria-invalid"));
        showError("");
      }, { signal: request.signal });
      form.addEventListener("submit", async event => {
        event.preventDefault();
        if (!revision || busy || !active()) return;
        const body = { revision };
        for (const { name, input, normalize, valid, invalid } of controls) {
          const value = normalize(input.value);
          if (!valid(value)) {
            showError(invalid);
            retry.hidden = true;
            input.setAttribute("aria-invalid", "true");
            input.focus();
            return;
          }
          body[name] = value;
        }
        loading(true);
        button.textContent = "保存中…";
        showError("");
        try {
          const result = await api.update({ body, signal: request.signal });
          if (!active()) return;
          if (controls.some(({ name }) => result?.[name] !== body[name])) throw Error("INVALID_RESPONSE");
          accept(result);
          UI.toast("已保存");
        } catch (failure) {
          if (active()) {
            revision = 0;
            showError(failure.code === "SETTINGS_CHANGED" ? "设置已更改，请重试读取" : "保存未确认，请重试读取");
          }
        } finally {
          if (active()) { loading(false); button.textContent = "保存"; }
        }
      }, { signal: request.signal });
      await load();
    }
    return { mount, unmount };
  },
};
