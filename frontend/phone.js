const Phone = (() => {
  const { $, escape: escapeHTML, toast, duration, day: dayLabel, time } = UI;
  const lineKey = "rykvo-voice-call-line-v1";
  const savedLine = UI.read(lineKey, "random");
  let selectedLine = Lines.valid(savedLine) ? savedLine : "random";
  let dialNumber = "";
  let phoneView = "dialer";
  let callHistory;
  try {
    const stored = localStorage.getItem("rykvo-voice-calls-v1");
    callHistory = stored === null ? [] : JSON.parse(stored);
    if (!Array.isArray(callHistory)) throw new Error("Invalid records");
    callHistory = callHistory.filter(
      (item) =>
        item &&
        typeof item.id === "string" &&
        typeof item.number === "string" &&
        /^[0-9+*#]+$/.test(item.number) &&
        ["incoming", "outgoing", "missed", "cancelled"].includes(item.kind) &&
        Number.isFinite(item.at) &&
        Number.isFinite(item.duration) &&
        item.duration >= 0,
    );
  } catch {
    callHistory = [];
  }
  function renderHistory() {
    const list = $("#history-list");
    if (!list) return;
    const rows = callHistory;
    $("#history-count").textContent = `${rows.length} 条`;
    list.innerHTML =
      Countries.group(rows)
        .map(([code, items]) => {
          let previousDay = "";
          return `<h3 class="history-day">${code ? `+${code}` : "未标注区号"}</h3>${items
            .map((item) => {
              const day = dayLabel(item.at),
                divider =
                  day !== previousDay
                    ? `<h3 class="history-day">${day}</h3>`
                    : "";
              previousDay = day;
              const kinds = {
                incoming: "来电",
                outgoing: "呼出",
                missed: "未接来电",
                cancelled: "已取消",
              };
              const title = Countries.format(item.number);
              const country = Countries.country(item.number);
              return `${divider}<article class="call-record ${item.kind === "missed" ? "is-missed" : ""}" data-call-id="${escapeHTML(item.id)}" tabindex="0"><span class="history-avatar" aria-hidden="true"><img src="assets/handset.png" alt=""></span><div class="history-person"><strong>${escapeHTML(title)}</strong><p>${country ? escapeHTML(country) + " · " : ""}${item.kind === "incoming" ? "↙" : item.kind === "missed" ? "↙" : "↗"} ${kinds[item.kind]}${item.kind === "incoming" || item.kind === "outgoing" ? " · " + duration(item.duration) : ""}</p></div><time datetime="${new Date(item.at).toISOString()}">${time(item.at)}</time><button class="redial-button" data-redial="${escapeHTML(item.id)}" aria-label="回拨${escapeHTML(title)}"><img src="assets/handset.png" alt=""></button></article>`;
            })
            .join("")}`;
        })
        .join("") || '<p class="history-empty">暂无通话</p>';
  }
  function removeHistory(id) {
    const next = id ? callHistory.filter((item) => item.id !== id) : [];
    if (!UI.write("rykvo-voice-calls-v1", next)) return;
    callHistory = next;
    renderHistory();
    toast(id ? "通话记录已删除" : "通话记录已全部删除");
  }
  function prune(cutoff) {
    // SIP 与电话共用存储，保留不属于电话列表的数据结构。
    const stored = UI.read("rykvo-voice-calls-v1", callHistory);
    const records = Array.isArray(stored) ? stored : callHistory;
    const next = records.filter((item) => !Cleanup.expired(item, cutoff));
    const kept = callHistory.filter((item) => !Cleanup.expired(item, cutoff));
    if (next.length === records.length && kept.length === callHistory.length)
      return true;
    if (!UI.write("rykvo-voice-calls-v1", next)) return false;
    callHistory = kept;
    renderHistory();
    return true;
  }
  document.addEventListener("contextmenu", (event) => {
    if (!event.target.closest("#history-list")) return;
    const id = event.target.closest("[data-call-id]")?.dataset.callId;
    const record = callHistory.find((item) => item.id === id);
    ContextMenu.open(event, [
      {
        label: "复制",
        disabled: !record,
        action: () => {
          if (record) return UI.copy(record.number);
        },
      },
      {
        label: "删除",
        danger: true,
        disabled: !id,
        action: () =>
          ContextMenu.confirm("删除这条通话记录？", () => removeHistory(id)),
      },
      {
        label: "全部删除",
        danger: true,
        disabled: !callHistory.length,
        action: () =>
          ContextMenu.confirm(
            "删除全部通话记录？",
            () => removeHistory(),
            "全部删除",
          ),
      },
    ]);
  });
  function setPhoneView(view) {
    phoneView = view;
    const workspace = $(".phone-workspace");
    if (!workspace) return;
    workspace.dataset.view = view;
    document
      .querySelectorAll("[data-phone-view]")
      .forEach((button) =>
        button.setAttribute(
          "aria-pressed",
          String(button.dataset.phoneView === view),
        ),
      );
  }
  function phone() {
    return `<div class="page-head phone-heading"><h1>电话</h1><div class="phone-view-switch" role="group" aria-label="电话页面视图"><button data-phone-view="dialer" aria-pressed="${phoneView === "dialer"}">拨号键盘</button><button data-phone-view="history" aria-pressed="${phoneView === "history"}">通话</button></div></div>
<div class="phone-workspace" data-view="${phoneView}"><section class="dialer-panel" aria-label="拨号"><section class="dialer" aria-label="拨号键盘">
  <div class="dialer-display"><label class="sr-only" for="dial-number">电话号码</label><input id="dial-number" type="tel" inputmode="tel" autocomplete="off" placeholder="输入号码" aria-describedby="call-status"><p id="call-status" role="status" aria-live="polite"></p></div>
  <div class="dial-line">${Lines.select("call-line", "拨出号码", selectedLine)}</div>
  <div class="dial-grid">${[
    ["1", ""],
    ["2", "ABC"],
    ["3", "DEF"],
    ["4", "GHI"],
    ["5", "JKL"],
    ["6", "MNO"],
    ["7", "PQRS"],
    ["8", "TUV"],
    ["9", "WXYZ"],
    ["*", ""],
    ["0", "+"],
    ["#", ""],
  ]
    .map(
      ([key, letters]) =>
        `<button class="dial-key ${key === "*" || key === "#" ? "dial-symbol" : ""}" data-digit="${key}" aria-label="${key}"><span>${key === "*" ? '<svg class="dial-asterisk" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3v18M4.2 7.5l15.6 9M4.2 16.5l15.6-9" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>' : key}</span>${key === "*" || key === "#" ? "" : `<small aria-hidden="true">${letters || "&nbsp;"}</small>`}</button>`,
    )
    .join("")}</div>
  <div class="dial-actions"><button class="dial-plus" data-action="dial-plus" aria-label="添加国际区号加号">+</button><button class="call-button" data-action="dial-call" aria-label="拨打电话"><img src="assets/handset.png" alt=""></button><button class="dial-delete" data-action="dial-delete" aria-label="删除最后一位">⌫</button></div>
</section></section><section class="history-panel" aria-label="通话"><div class="history-heading"><div><h2>通话</h2></div><span id="history-count"></span></div><div id="history-list" class="scroll-area" aria-live="polite"></div></section></div>`;
  }
  function updateDialer() {
    const input = $("#dial-number");
    if (!input) return;
    input.value = Countries.format(dialNumber);
    input.style.setProperty("--number-length", Math.max(1, input.value.length));
    $("#call-line").value = selectedLine;
    $("#call-status").textContent = Countries.country(dialNumber);
    const call = $(".call-button");
    call.disabled = !dialNumber.replace(/[^0-9]/g, "");
    call.setAttribute("aria-label", "拨打电话");
  }
  function editDialNumber(key) {
    const input = $("#dial-number");
    if (!input) return;
    const next = UI.editDial(
      dialNumber,
      key,
      Countries.rawOffset(input.value, input.selectionStart),
      Countries.rawOffset(input.value, input.selectionEnd),
    );
    dialNumber = next.value;
    updateDialer();
    const caret = Countries.displayOffset(input.value, next.caret);
    input.setSelectionRange(caret, caret);
  }
  function requestCall() {
    toast(/[0-9]/.test(dialNumber) ? "通话暂不可用" : "请先输入电话号码");
  }

  let heldPress = null,
    ignoredClick = null;
  function pressControl(target) {
    if (target.closest('[data-digit="0"]')) return "zero";
    if (target.closest('[data-action="dial-delete"]')) return "delete";
    return null;
  }
  function startPress(control, source) {
    if (heldPress) return;
    heldPress = {
      control,
      source,
      held: false,
      timer: setTimeout(() => {
        if (!heldPress) return;
        heldPress.held = true;
        editDialNumber(control === "zero" ? "+" : "clear");
      }, 500),
    };
  }
  function endPress(commit) {
    if (!heldPress) return;
    const press = heldPress;
    clearTimeout(press.timer);
    heldPress = null;
    if (commit && !press.held)
      editDialNumber(press.control === "zero" ? "0" : "delete");
  }
  function mount() {
    updateDialer();
    renderHistory();
  }
  function unmount() {
    endPress(false);
  }
  document.addEventListener("pointerdown", (e) => {
    const control = pressControl(e.target);
    if (e.button !== 0 || !control) return;
    ignoredClick = null;
    startPress(control, "pointer");
  });
  for (const type of ["pointerup", "pointercancel"]) {
    document.addEventListener(type, () => {
      if (heldPress?.source !== "pointer") return;
      ignoredClick = heldPress.control;
      endPress(type === "pointerup");
    });
  }
  document.addEventListener("contextmenu", (e) => {
    if (pressControl(e.target)) e.preventDefault();
  });
  document.addEventListener("click", (e) => {
    if (!e.target.closest(".phone-workspace,.phone-heading")) return;
    const control = pressControl(e.target);
    if (control && ignoredClick === control) {
      ignoredClick = null;
      return;
    }
    const key = e.target.closest("[data-digit]");
    if (key) {
      editDialNumber(key.dataset.digit);
      return;
    }
    const view = e.target.closest("[data-phone-view]");
    if (view) {
      setPhoneView(view.dataset.phoneView);
      return;
    }
    const redial = e.target.closest("[data-redial]");
    if (redial) {
      const record = callHistory.find((r) => r.id === redial.dataset.redial);
      if (record) {
        dialNumber = record.number;
        setPhoneView("dialer");
        requestCall();
      }
      return;
    }
    const action = e.target.closest("[data-action]")?.dataset.action;
    if (action === "dial-call") requestCall();
    if (action === "dial-delete") editDialNumber("delete");
    if (action === "dial-plus") editDialNumber("+");
  });
  document.addEventListener("change", (event) => {
    if (event.target.id !== "call-line") return;
    const value = event.target.value;
    if (Lines.valid(value) && UI.write(lineKey, value))
      selectedLine = value;
    event.target.value = selectedLine;
  });
  document.addEventListener("input", (e) => {
    if (e.target.id !== "dial-number") return;
    const caret = e.target.value
      .slice(0, e.target.selectionStart)
      .replace(/[^0-9+*#]/g, "").length;
    dialNumber = e.target.value.replace(/[^0-9+*#]/g, "");
    updateDialer();
    const position = Countries.displayOffset(e.target.value, caret);
    e.target.setSelectionRange(position, position);
  });
  document.addEventListener("keydown", (e) => {
    if (
      !$("#dial-number") ||
      $("#dialog").open ||
      e.ctrlKey ||
      e.metaKey ||
      e.altKey
    )
      return;
    if (
      e.target.closest("input,textarea,select,.line-picker,.line-trigger") &&
      e.target.id !== "dial-number"
    )
      return;
    if (e.isComposing) return;
    if (
      e.key === "Enter" &&
      (e.target.id === "dial-number" || e.target === document.body)
    ) {
      e.preventDefault();
      requestCall();
      return;
    }
    if (e.key === "0") {
      e.preventDefault();
      if (!e.repeat) startPress("zero", "keyboard");
      return;
    }
    if (/^[1-9+*#]$/.test(e.key)) {
      e.preventDefault();
      editDialNumber(e.key);
      return;
    }
    if (
      e.target.id === "dial-number" &&
      ["Backspace", "Delete"].includes(e.key)
    ) {
      e.preventDefault();
      editDialNumber(e.key === "Backspace" ? "backspace" : "forward-delete");
    } else if (e.key === "Backspace") {
      e.preventDefault();
      editDialNumber("delete");
    }
  });
  document.addEventListener("keyup", (e) => {
    if (e.key === "0" && heldPress?.source === "keyboard") {
      e.preventDefault();
      endPress(true);
    }
  });
  window.addEventListener("blur", () => endPress(false));
  return { render: phone, mount, unmount, prune };
})();
