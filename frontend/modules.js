const Modules = (() => {
  const statuses = {
    all: "全部",
    online: "在线",
    offline: "离线",
    error: "异常",
  };
  const signals = {
    none: "无服务",
    wifi: "Wi-Fi 通话",
    mobile: "中国移动",
    unicom: "中国联通",
    telecom: "中国电信",
  };
  const { items, labels, labelKey, validLabel } = ModuleData;
  let filter = "all";
  let query = "";
  let unsubscribe = null;
  function count(records) {
    return records.reduce(
      (total, item) => {
        total.all++;
        if (Object.hasOwn(total, item.status) && item.status !== "all")
          total[item.status]++;
        return total;
      },
      { all: 0, online: 0, offline: 0, error: 0 },
    );
  }
  const order = new Intl.Collator("zh-CN", { numeric: true });
  function select(records, status, term = "") {
    const text = term.trim().toLowerCase();
    const compact = text.replace(/[\s()-]/g, "");
    const moduleNumber = /^\d{1,2}$/.test(text) ? Number(text) : null;
    return records
      .filter((item) => {
        if (status !== "all" && item.status !== status) return false;
        if (!text) return true;
        if (moduleNumber !== null) {
          const suffix = item.name?.match(/\d+$/)?.[0];
          return suffix !== undefined && Number(suffix) === moduleNumber;
        }
        const numbers = [
          item.number,
          item.hardware?.iccid,
          ...(item.sims || []).flatMap((sim) => [sim.number, sim.iccid]),
        ].filter(Boolean);
        return (
          item.name.toLowerCase().includes(text) ||
          (item.label || "").toLowerCase().includes(text) ||
          Boolean(
            compact &&
            numbers.some((number) =>
              number.replace(/[\s()-]/g, "").includes(compact),
            ),
          )
        );
      })
      .sort(
        (a, b) =>
          order.compare(a.name || "", b.name || "") ||
          order.compare(a.number || "", b.number || ""),
      );
  }
  function badge(status, issue) {
    const text = issue === "RECOVERING" ? "正在恢复" : status === "error" && issue && issue !== "IDENTITY_CONFLICT" ? "读取异常" : statuses[status] || "未知";
    return `<span class="module-status" data-status="${UI.escape(status)}" title="${UI.escape(ModuleData.issueText(issue))}"><i aria-hidden="true"></i>${UI.escape(text)}</span>`;
  }
  function signalLabel(value, item) {
    if (item?.managed) {
      if (item.status !== "online") return "—";
      if (item.kind === "reader") return "SIM 读卡器";
      if (item.hardware?.registration === "denied") return "注册被拒绝";
      if (item.signal === "none") return "无服务";
      return [item.hardware?.operator || item.hardware?.plmn, item.hardware?.technology].filter(Boolean).join(" · ") || "无服务";
    }
    return Object.hasOwn(signals, value) ? signals[value] : signals.none;
  }
  function rows(records) {
    return records.length
      ? records
          .map(
            (item) =>
              `<tr data-module-row="${UI.escape(item.id)}"><th scope="row">${UI.escape(item.label || item.name)}</th><td class="module-number">${UI.escape(ModuleData.identity(item.number, item.hardware?.iccid).caption)}</td><td>${badge(item.status, item.issue)}</td><td class="module-signal">${UI.escape(signalLabel(item.signal, item))}</td><td><button type="button" class="text-button module-detail" data-module-detail="${UI.escape(item.id)}" aria-label="查看${UI.escape(item.name)}详情">详情</button></td></tr>`,
          )
          .join("")
      : `<tr><td colspan="5" class="data-empty">${ModuleData.issue ? "读取异常，正在重试" : ModuleData.connected() && !ModuleData.loaded ? "正在读取" : "暂无模块"}</td></tr>`;
  }
  function render() {
    filter = "all";
    query = "";
    const counts = count(items);
    return `<section class="modules-page"><div class="page-head"><h1>模块</h1></div><div class="module-summary" role="group" aria-label="模块状态筛选">${Object.entries(
      statuses,
    )
      .map(
        ([key, label]) =>
          `<button type="button" class="module-stat" data-module-filter="${key}" aria-pressed="${key === filter}"><span class="module-stat-label"><i aria-hidden="true"></i>${label}</span><strong>${counts[key]}</strong></button>`,
      )
      .join(
        "",
      )}</div><div class="data-toolbar"><label class="data-search"><svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="8.5" cy="8.5" r="5.5"/><path d="m13 13 4 4"/></svg><input id="module-search" type="search" aria-label="搜索模块或号码" placeholder="搜索模块或号码" autocomplete="off"></label></div><div class="data-table-wrap" role="region" aria-label="模块列表滚动区域" tabindex="0"><table class="data-table" aria-label="模块列表"><colgroup><col class="module-col-name"><col class="module-col-number"><col class="module-col-status"><col class="module-col-signal"><col class="module-col-action"></colgroup><thead><tr><th scope="col">模块</th><th scope="col">号码</th><th scope="col">状态</th><th scope="col">信号</th><th scope="col">操作</th></tr></thead><tbody id="module-rows">${rows(select(items, filter))}</tbody></table></div><p id="module-service" class="field-error" role="status" hidden></p><span id="module-result" class="sr-only" role="status" aria-live="polite"></span></section>`;
  }
  function updateRows(resetScroll = true) {
    const visible = select(items, filter, query);
    const body = document.getElementById("module-rows");
    if (!body) return;
    const html = rows(visible);
    if (!body.querySelectorAll || !visible.length) {
      if (body.innerHTML !== html) body.innerHTML = html;
    } else {
      const previous = new Map([...body.querySelectorAll("tr[data-module-row]")].map((row) => [row.dataset.moduleRow, row]));
      if (!previous.size) body.replaceChildren();
      visible.forEach((item, index) => {
        const template = document.createElement("template");
        template.innerHTML = `<table><tbody>${rows([item])}</tbody></table>`;
        const fresh = template.content.querySelector("tr");
        const row = previous.get(item.id) || fresh;
        if (row !== fresh) {
          [...fresh.children].forEach((cell, n) => {
            if (row.children[n].innerHTML !== cell.innerHTML) row.children[n].innerHTML = cell.innerHTML;
          });
        }
        if (body.children[index] !== row) body.insertBefore(row, body.children[index] || null);
        previous.delete(item.id);
      });
      for (const row of previous.values()) row.remove();
    }
    const counts = count(items);
    document.querySelectorAll("[data-module-filter]").forEach((button) => {
      const value = button.querySelector?.("strong");
      if (value) value.textContent = counts[button.dataset.moduleFilter];
    });
    const service = document.getElementById("module-service");
    if (service) {
      service.hidden = !ModuleData.issue || !items.length;
      service.textContent = ModuleData.issue === "IDENTITY_CONFLICT" ? "设备身份冲突，请检查模块" : "模块状态读取异常，正在重试";
    }
    document.getElementById("module-result").textContent =
      `${statuses[filter]}，${visible.length} 个模块`;
    if (resetScroll)
      document.querySelector(".modules-page .data-table-wrap").scrollTop = 0;
  }
  document.addEventListener("click", (event) => {
    const button = event.target.closest("[data-module-filter]");
    if (button) {
      const value = button.dataset.moduleFilter;
      if (!Object.hasOwn(statuses, value) || value === filter) return;
      filter = value;
      document
        .querySelectorAll("[data-module-filter]")
        .forEach((card) =>
          card.setAttribute(
            "aria-pressed",
            String(card.dataset.moduleFilter === filter),
          ),
        );
      updateRows();
      return;
    }
    const detail = event.target.closest("[data-module-detail]");
    if (!detail) return;
    const item = items.find(
      (record) => record.id === detail.dataset.moduleDetail,
    );
    if (!item) return;
    Cellular.open(item);
  });
  document.addEventListener("contextmenu", (event) => {
    const row = event.target.closest("[data-module-row]");
    const item = items.find((record) => record.id === row?.dataset.moduleRow);
    if (!item) return;
    ContextMenu.open(event, [
      {
        label: "标签",
        action: () => {
          UI.modal(
            "标签",
            `<form id="module-label-form" class="settings-form" data-module-id="${UI.escape(item.id)}" novalidate><div class="form-fields">${Forms.field({ prefix: "module", name: "label", label: "标签", value: item.label || item.name, placeholder: item.name, maxLength: 20 })}</div><div class="form-footer"><button type="submit" class="primary">完成</button></div></form>`,
          );
          document.getElementById("module-label").select();
        },
      },
      { label: "详情", action: () => Cellular.open(item) },
    ]);
  });
  document.addEventListener("submit", async (event) => {
    const form = event.target;
    if (form.id !== "module-label-form") return;
    event.preventDefault();
    const item = items.find((record) => record.id === form.dataset.moduleId);
    if (!item) return;
    const label = form.elements.namedItem("label").value.trim();
    if (
      Forms.report(
        form,
        label && !validLabel(label)
          ? { field: "label", message: "标签最多 20 个字符" }
          : null,
      )
    )
      return;
    if (label && ModuleData.duplicate(item.id, label)) {
      Forms.report(form, { field: "label", message: "标签已存在" });
      return;
    }
    if (ModuleData.connected()) {
      const button = form.querySelector('button[type="submit"]');
      if (button.disabled) return;
      button.disabled = true;
      try {
        await ModuleData.saveLabel(item, label);
        if (form.isConnected) document.getElementById("dialog").close();
        UI.toast("标签已保存");
      } catch (error) {
        if (form.isConnected) Forms.report(form, { field: "label", message: error.code === "LABEL_EXISTS" ? "标签已存在" : "保存失败，请重试" });
      } finally { button.disabled = false; }
      return;
    }
    const next = { ...labels };
    if (!label || label === item.name) delete next[item.id];
    else next[item.id] = label;
    if (!UI.write(labelKey, next)) return;
    if (next[item.id]) labels[item.id] = next[item.id];
    else delete labels[item.id];
    item.label = next[item.id] || "";
    updateRows(false);
    document.getElementById("dialog").close();
    UI.toast("标签已保存");
  });
  function search(event) {
    if (
      event.target.id !== "module-search" ||
      event.isComposing ||
      query === event.target.value
    )
      return;
    query = event.target.value;
    updateRows();
  }
  document.addEventListener("input", search);
  document.addEventListener("compositionend", search);
  return {
    render, count, select, rows,
    get total() { return items.length; },
    mount() { unsubscribe?.(); unsubscribe = ModuleData.subscribe(() => updateRows(false)); updateRows(false); },
    unmount() { unsubscribe?.(); unsubscribe = null; },
  };
})();
