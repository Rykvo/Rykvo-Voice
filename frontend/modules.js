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
          ...(item.sims || []).map((sim) => sim.number),
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
  function badge(status) {
    return `<span class="module-status" data-status="${UI.escape(status)}"><i aria-hidden="true"></i>${UI.escape(statuses[status] || "未知")}</span>`;
  }
  function signalLabel(value) {
    return Object.hasOwn(signals, value) ? signals[value] : signals.none;
  }
  function rows(records) {
    return records.length
      ? records
          .map(
            (item) =>
              `<tr data-module-row="${UI.escape(item.id)}"><th scope="row">${UI.escape(item.label || item.name)}</th><td class="module-number">${UI.escape(Countries.format(item.number))}</td><td>${badge(item.status)}</td><td class="module-signal">${signalLabel(item.signal)}</td><td><button type="button" class="text-button module-detail" data-module-detail="${UI.escape(item.id)}" aria-label="查看${UI.escape(item.name)}详情">详情</button></td></tr>`,
          )
          .join("")
      : '<tr><td colspan="5" class="data-empty">暂无模块</td></tr>';
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
      )}</div><div class="data-toolbar"><label class="data-search"><svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="8.5" cy="8.5" r="5.5"/><path d="m13 13 4 4"/></svg><input id="module-search" type="search" aria-label="搜索模块或号码" placeholder="搜索模块或号码" autocomplete="off"></label></div><div class="data-table-wrap" role="region" aria-label="模块列表滚动区域" tabindex="0"><table class="data-table" aria-label="模块列表"><colgroup><col class="module-col-name"><col class="module-col-number"><col class="module-col-status"><col class="module-col-signal"><col class="module-col-action"></colgroup><thead><tr><th scope="col">模块</th><th scope="col">号码</th><th scope="col">状态</th><th scope="col">信号</th><th scope="col">操作</th></tr></thead><tbody id="module-rows">${rows(select(items, filter))}</tbody></table></div><span id="module-result" class="sr-only" role="status" aria-live="polite"></span></section>`;
  }
  function updateRows(resetScroll = true) {
    const visible = select(items, filter, query);
    document.getElementById("module-rows").innerHTML = rows(visible);
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
  document.addEventListener("submit", (event) => {
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
  return { render, total: items.length, count, select, rows };
})();
