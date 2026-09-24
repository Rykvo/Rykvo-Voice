// 会话列表、聊天内容与输入栏独立更新。
const Messages = (() => {
  const key = "rykvo-voice-messages-v1";
  const { $, escape: esc, toast } = UI;
  const senderKey = "rykvo-voice-sms-sender-v1";
  const savedSender = UI.read(senderKey, "random");
  let defaultSender = Lines.valid(savedSender) ? savedSender : "random";
  function senderId(thread = current()) {
    if (!thread) return defaultSender;
    const id =
      thread?.senderId ||
      thread?.messages.findLast(
        (message) => message.mine && Lines.recorded(message.senderId),
      )?.senderId;
    return Lines.recorded(id) ? id : "";
  }
  function senderHTML() {
    if (active !== "new") return "";
    return `<div class="msg-recipient msg-sender"><label for="msg-from">发件人：</label>${Lines.select("msg-from", "发件人", senderId())}</div>`;
  }
  const avatar = () => '<span class="msg-avatar" aria-hidden="true"></span>';
  const safeImage = (value) =>
    typeof value === "string" &&
    /^data:image\/(png|jpeg|webp|gif);base64,[A-Za-z0-9+/=]+$/.test(value);
  let threads;
  try {
    threads = JSON.parse(localStorage.getItem(key) || "null");
    if (!Array.isArray(threads)) throw 0;
    threads = threads
      .filter(
        (t) =>
          t &&
          typeof t.id === "string" &&
          /^\+?\d{3,20}$/.test(t.number) &&
          Array.isArray(t.messages),
      )
      .map((t) => ({
        ...t,
        messages: t.messages
          .filter(
            (m) => m && typeof m.text === "string" && Number.isFinite(m.at),
          )
          .map((m) => ({ ...m, image: safeImage(m.image) ? m.image : null })),
      }));
  } catch {
    threads = [];
  }
  let active = threads[0]?.id || "",
    view = "list",
    query = "",
    drafts = {},
    newNumber = "",
    attachment = null;
  function save() {
    return UI.write(key, threads);
  }
  function current() {
    return threads.find((t) => t.id === active);
  }
  function time(at) {
    const d = new Date(at);
    return d.toDateString() === new Date().toDateString()
      ? d.toLocaleTimeString("zh-CN", {
          hour: "2-digit",
          minute: "2-digit",
          hour12: false,
        })
      : d.toLocaleDateString("zh-CN", { month: "numeric", day: "numeric" });
  }
  function listHTML() {
    const rows = threads
      .filter(
        (t) =>
          t.number.includes(query.replace(/\s/g, "")) ||
          Countries.regionName(t.region).includes(query) ||
          t.messages.some((m) =>
            m.text.toLowerCase().includes(query.toLowerCase()),
          ),
      )
      .sort(
        (a, b) => (b.messages.at(-1)?.at || 0) - (a.messages.at(-1)?.at || 0),
      );
    return (
      Countries.group(rows)
        .map(
          ([code, items]) =>
            `<h2 class="msg-code-group">${code ? `+${code}` : "未标注区号"}</h2>${items
              .map((t) => {
                const last = t.messages.at(-1);
                return `<button class="msg-thread ${active === t.id ? "selected" : ""}" data-msg-thread="${esc(t.id)}" aria-pressed="${active === t.id}"><span class="msg-unread ${t.unread ? "visible" : ""}" aria-label="${t.unread ? "未读" : ""}"></span>${avatar()}<span class="msg-thread-copy"><span class="msg-thread-top"><strong>${esc(Countries.format(t.number, t.region))}</strong><time>${last ? time(last.at) : ""}</time><span aria-hidden="true">›</span></span><span class="msg-preview">${esc(last?.text || (last?.image ? "照片" : ""))}</span></span></button>`;
              })
              .join("")}`,
        )
        .join("") ||
      `<p class="msg-empty">${query ? "没有找到信息" : "暂无信息"}</p>`
    );
  }
  function headerHTML() {
    return active === "new"
      ? `<button class="msg-back" data-msg-action="back" aria-label="返回会话列表">‹ 信息</button><h2 class="msg-new-title">新信息</h2><button class="msg-text-button" data-msg-action="cancel">取消</button>`
      : `<button class="msg-back" data-msg-action="back" aria-label="返回会话列表">‹ 信息</button><button class="msg-contact" data-msg-action="copy" aria-label="复制号码">${avatar()}<span>${esc(Countries.format(current()?.number || "", current()?.region))}</span></button>`;
  }
  function render() {
    const empty = active !== "new" && !current();
    return `<div class="messages-app" data-view="${view}"><aside class="msg-sidebar" aria-label="会话列表"><header class="msg-list-header"><h1>信息</h1><button class="msg-icon-button" data-msg-action="compose" aria-label="新建信息"><img src="assets/compose.png" alt=""></button></header><label class="msg-search"><span aria-hidden="true"><svg viewBox="0 0 20 20"><circle cx="8.5" cy="8.5" r="5.5" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="m13 13 4 4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg></span><input id="msg-search" type="search" placeholder="搜索" aria-label="搜索信息" value="${esc(query)}"></label><div class="msg-thread-list scroll-area" id="msg-thread-list">${listHTML()}</div></aside><section class="msg-chat" aria-label="信息对话">${empty ? '<div class="msg-empty-state">暂无信息</div>' : `<header class="msg-chat-header" id="msg-chat-header">${headerHTML()}</header><div class="msg-recipient" id="msg-recipient" ${active === "new" ? "" : "hidden"}><label for="msg-to">收件人：</label><input id="msg-to" type="tel" inputmode="tel" placeholder="手机号码" aria-label="收件人手机号" value="${esc(newNumber)}" maxlength="21"></div>${senderHTML()}<div class="msg-transcript scroll-area" id="msg-transcript" role="log" aria-label="聊天内容" aria-live="polite"></div><div class="msg-composer-area"><div class="msg-attachment" id="msg-attachment" hidden></div><form class="msg-composer" id="ipad-message-form"><button type="button" class="msg-attach-button" data-msg-action="attach" aria-label="添加照片"><img src="assets/message-plus.png" alt=""></button><input hidden id="msg-file" type="file" accept="image/png,image/jpeg,image/webp,image/gif"><div class="msg-input-wrap"><textarea id="msg-input" aria-label="信息内容" placeholder="短信" rows="1" maxlength="2000">${esc(drafts[active] || "")}</textarea><button class="msg-send" type="submit" aria-label="发送信息" disabled><img src="assets/message-send.png" alt=""></button></div></form></div>`}</section></div>`;
  }
  function bubbleHTML(message, previous) {
    const date = new Date(message.at),
      isNewDay =
        !previous ||
        date.toDateString() !== new Date(previous.at).toDateString();
    return `${isNewDay ? `<div class="msg-date">${UI.day(message.at)} ${UI.time(message.at)}</div>` : ""}<div class="msg-line ${message.mine ? "outgoing" : "incoming"}"><div class="msg-bubble ${message.image ? "with-image" : ""}">${message.image ? `<img src="${message.image}" alt="信息中的照片">` : ""}${message.text ? `<span>${esc(message.text)}</span>` : ""}</div></div>`;
  }
  function scrollToLatest() {
    const node = $("#msg-transcript");
    if (node) node.scrollTop = node.scrollHeight;
  }
  function transcript() {
    const node = $("#msg-transcript");
    if (!node) return;
    const items = current()?.messages || [];
    node.innerHTML = items
      .map((message, index) => bubbleHTML(message, items[index - 1]))
      .join("");
    scrollToLatest();
  }
  function renderAttachment() {
    const area = $("#msg-attachment");
    if (!area) return;
    area.hidden = !attachment;
    area.innerHTML = attachment
      ? `<img src="${attachment.data}" alt="待发送照片"><button type="button" data-msg-action="remove-photo" aria-label="移除照片">×</button>`
      : "";
  }
  function updateSend() {
    const input = $("#msg-input"),
      btn = $(".msg-send");
    if (!input || !btn) return;
    btn.disabled = !input.value.trim() && !attachment;
    input.style.height = "auto";
    input.style.height = Math.min(input.scrollHeight, 120) + "px";
  }
  function mount() {
    if (!$(".messages-app")) return;
    transcript();
    renderAttachment();
    updateSend();
  }
  function showThread(id) {
    active = id;
    view = "chat";
    attachment = null;
    const t = current();
    if (t) {
      t.unread = false;
      save();
    }
    refresh();
  }
  function refresh() {
    const root = $(".messages-app");
    if (!root) return;
    const scrollTop = $("#msg-thread-list").scrollTop;
    root.outerHTML = render();
    mount();
    $("#msg-thread-list").scrollTop = scrollTop;
  }
  function send() {
    const input = $("#msg-input");
    const text = input.value.trim();
    if (!text && !attachment) return;
    const isNew = active === "new";
    let t = current();
    let number;
    if (isNew) {
      number = $("#msg-to").value.replace(/[\s()-]/g, "");
      if (!/^\+?\d{3,20}$/.test(number)) {
        toast("请输入有效的手机号码");
        $("#msg-to").focus();
        return;
      }
      t = threads.find((item) => item.number === number);
    }
    const selectedSender = Lines.resolve(t ? senderId(t) : defaultSender);
    if (!selectedSender) {
      toast("暂无可用模块，请选择在线模块");
      return;
    }
    if (isNew) {
      if (!t) {
        t = { id: UI.id(), number, unread: false, messages: [] };
        threads.unshift(t);
      }
      active = t.id;
      newNumber = "";
      delete drafts.new;
    }
    if (!t) return;
    t.senderId = selectedSender;
    const previous = t.messages.at(-1),
      message = {
        id: UI.id(),
        text,
        mine: true,
        senderId: t.senderId,
        at: Date.now(),
        image: attachment?.data || null,
      };
    t.messages.push(message);
    t.unread = false;
    drafts[active] = "";
    attachment = null;
    query = "";
    save();
    if (isNew) refresh();
    else {
      input.value = "";
      $("#msg-search").value = "";
      $("#msg-thread-list").innerHTML = listHTML();
      $("#msg-transcript").insertAdjacentHTML(
        "beforeend",
        bubbleHTML(message, previous),
      );
      renderAttachment();
      updateSend();
      scrollToLatest();
    }
    $("#msg-input").focus();
  }
  function removeThreads(id) {
    const index = threads.findIndex((thread) => thread.id === id);
    const next = id ? threads.filter((thread) => thread.id !== id) : [];
    if (!UI.write(key, next)) return;
    threads = next;
    const activeRemoved = !id || active === id;
    if (id) delete drafts[id];
    else drafts = {};
    if (activeRemoved) {
      active =
        threads[Math.min(Math.max(index, 0), threads.length - 1)]?.id || "";
      attachment = null;
      newNumber = "";
      if (!threads.length) view = "list";
      refresh();
    } else if ($("#msg-thread-list")) {
      $("#msg-thread-list").innerHTML = listHTML();
    }
    toast(id ? "会话已删除" : "会话已全部删除");
  }
  function prune(cutoff) {
    let changed = false;
    const next = threads.flatMap((thread) => {
      const messages = thread.messages.filter(
        (message) => !Cleanup.expired(message, cutoff),
      );
      if (messages.length === thread.messages.length) return [thread];
      changed = true;
      // 空会话中的草稿和待发送照片仍保留。
      if (
        !messages.length &&
        !drafts[thread.id] &&
        !(active === thread.id && attachment)
      )
        return [];
      return [
        {
          ...thread,
          messages,
          unread: thread.unread && messages.some((message) => !message.mine),
        },
      ];
    });
    if (!changed) return true;
    if (!UI.write(key, next)) return false;
    threads = next;
    if (active !== "new" && !current()) {
      active = threads[0]?.id || "";
      if (!active) view = "list";
      refresh();
    } else {
      const list = $("#msg-thread-list");
      if (list) list.innerHTML = listHTML();
      transcript();
    }
    return true;
  }
  document.addEventListener("contextmenu", (event) => {
    const list = event.target.closest("#msg-thread-list");
    if (!list) return;
    const id = event.target.closest("[data-msg-thread]")?.dataset.msgThread;
    const record = threads.find((item) => item.id === id);
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
          ContextMenu.confirm("删除这条会话？", () => removeThreads(id)),
      },
      {
        label: "全部删除",
        danger: true,
        disabled: !threads.length,
        action: () =>
          ContextMenu.confirm(
            "删除全部会话？",
            () => removeThreads(),
            "全部删除",
          ),
      },
    ]);
  });
  document.addEventListener("click", (e) => {
    if (!e.target.closest(".messages-app")) return;
    const thread = e.target.closest("[data-msg-thread]");
    if (thread) {
      showThread(thread.dataset.msgThread);
      return;
    }
    const action = e.target.closest("[data-msg-action]")?.dataset.msgAction;
    if (!action) return;
    if (action === "compose") {
      active = "new";
      view = "chat";
      newNumber = "";
      attachment = null;
      refresh();
      $("#msg-to").focus();
    }
    if (action === "back") {
      view = "list";
      $(".messages-app").dataset.view = view;
    }
    if (action === "cancel") {
      active = threads[0]?.id || "";
      view = "list";
      attachment = null;
      newNumber = "";
      refresh();
    }
    if (action === "attach") $("#msg-file").click();
    if (action === "remove-photo") {
      attachment = null;
      renderAttachment();
      updateSend();
    }
    if (action === "copy" && current()) UI.copy(current().number);
  });
  document.addEventListener("input", (e) => {
    if (e.target.id === "msg-input") {
      drafts[active] = e.target.value;
      updateSend();
    }
    if (e.target.id === "msg-to") newNumber = e.target.value;
    if (e.target.id === "msg-search") {
      query = e.target.value.trim();
      $("#msg-thread-list").innerHTML = listHTML();
    }
  });
  document.addEventListener("keydown", (e) => {
    if (
      e.target.id === "msg-input" &&
      e.key === "Enter" &&
      !e.shiftKey &&
      !e.isComposing
    ) {
      e.preventDefault();
      send();
    }
  });
  document.addEventListener("submit", (e) => {
    if (e.target.id === "ipad-message-form") {
      e.preventDefault();
      send();
    }
  });
  document.addEventListener("change", (e) => {
    if (e.target.id === "msg-from") {
      if (active !== "new") return;
      const id = e.target.value;
      if (Lines.valid(id) && UI.write(senderKey, id)) defaultSender = id;
      e.target.value = defaultSender;
      return;
    }
    if (e.target.id !== "msg-file") return;
    const file = e.target.files[0];
    if (!file) return;
    if (
      !["image/png", "image/jpeg", "image/webp", "image/gif"].includes(
        file.type,
      ) ||
      file.size > 2 * 1024 * 1024
    ) {
      toast("请选择 2 MB 以内的图片");
      e.target.value = "";
      return;
    }
    const target = active,
      reader = new FileReader();
    reader.onload = () => {
      if (active !== target || !$("#msg-attachment")) return;
      attachment = { data: reader.result };
      renderAttachment();
      updateSend();
    };
    reader.onerror = () => toast("照片读取失败，请重试");
    reader.readAsDataURL(file);
  });
  document.addEventListener(
    "load",
    (event) => {
      if (event.target.matches?.("#msg-transcript img")) scrollToLatest();
    },
    true,
  );
  return { render, mount, prune };
})();
