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
  const title = thread => thread?.name || Countries.format(thread?.number || "", thread?.region);
  const emptyReceived = message => message && !message.mine && message.state === "received" && !message.image && !message.text?.trim();
  function avatar(thread) {
    const item = MessageIdentity.avatar(thread?.number || "", thread?.name || "");
    return `<span class="msg-avatar avatar-${item.color}" aria-hidden="true">${esc(item.text)}</span>`;
  }
  let noteTarget = null, savingNote = false;
  function editNote() {
    const thread = current();
    if (!thread) return;
    noteTarget = {id:thread.id, remote:thread.remote, lineId:thread.lineId, number:thread.number};
    UI.modal("备注", `<form id="message-note-form"><div class="msg-note-toolbar"><button type="button" data-msg-note-cancel>取消</button><button type="submit">保存</button></div><div class="msg-note-person">${avatar(thread)}<div><strong>${esc(title(thread))}</strong><span>${esc(thread.number)}</span></div></div><label class="msg-note-field" for="msg-note-name">备注名称<input id="msg-note-name" name="name" value="${esc(thread.name || "")}" placeholder="例如：Rykvo" maxlength="48" autocomplete="off"><span id="msg-note-count">${Array.from(thread.name || "").length}/24</span></label></form>`);
    $("#msg-note-name").focus();
  }
  async function saveNote(form) {
    if (savingNote || !noteTarget) return;
    const target = noteTarget, name = $("#msg-note-name").value.trim();
    if (Array.from(name).length > 24) {toast("备注最多 24 个字");return;}
    savingNote = true;
    const button = form.querySelector('[type="submit"]'); button.disabled = true;
    try {
      if (target.remote) await MessageData.saveContact({lineId:target.lineId, number:target.number, name});
      else {
        const next = threads.map(t => t.id === target.id ? {...t, name} : t);
        if (!UI.write(key, next.filter(t => !t.remote))) return;
        threads = next;
        $("#msg-thread-list").innerHTML = listHTML();
        $("#msg-chat-header").innerHTML = headerHTML();
      }
      if (noteTarget === target && form.isConnected) $("#dialog").close();
    } catch { toast("备注未保存，请重试"); }
    finally { savingNote = false; button.disabled = false; }
  }
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
  let unsubscribe = null, sending = false, pendingSubmit = null;
  let active = threads[0]?.id || "",
    view = "list",
    query = "",
    drafts = {},
    newNumber = "",
    attachment = null;
  function save() {
    return UI.write(key, threads.filter(t => !t.remote));
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
          (t.name || "").toLowerCase().includes(query.toLowerCase()) ||
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
                return `<button class="msg-thread ${active === t.id ? "selected" : ""}" data-msg-thread="${esc(t.id)}" aria-pressed="${active === t.id}"><span class="msg-unread ${t.unread ? "visible" : ""}" aria-label="${t.unread ? "未读" : ""}"></span>${avatar(t)}<span class="msg-thread-copy"><span class="msg-thread-top"><strong>${t.name ? `<span class="msg-thread-name">${esc(t.name)}</span><span class="msg-thread-number">${esc(t.number)}</span>` : esc(title(t))}</strong><time>${last ? time(last.at) : ""}</time><span aria-hidden="true">›</span></span><span class="msg-preview">${esc(emptyReceived(last) ? "无文本内容" : last?.text || (last?.image ? "照片" : ""))}</span></span></button>`;
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
      : `<button class="msg-back" data-msg-action="back" aria-label="返回会话列表">‹ 信息</button><button class="msg-contact" data-msg-action="note" aria-label="编辑备注">${avatar(current())}<span class="msg-contact-name">${esc(title(current()))}</span>${current()?.name ? `<small class="msg-contact-number">${esc(current().number)}</small>` : ""}</button>`;
  }
  function render() {
    const empty = active !== "new" && !current();
    return `<div class="messages-app" data-view="${view}"><aside class="msg-sidebar" aria-label="会话列表"><header class="msg-list-header"><h1>信息</h1><button class="msg-icon-button" data-msg-action="compose" aria-label="新建信息"><img src="assets/compose.png" alt=""></button></header><label class="msg-search"><span aria-hidden="true"><svg viewBox="0 0 20 20"><circle cx="8.5" cy="8.5" r="5.5" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="m13 13 4 4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg></span><input id="msg-search" type="search" placeholder="搜索" aria-label="搜索信息" value="${esc(query)}"></label><div class="msg-thread-list scroll-area" id="msg-thread-list">${listHTML()}</div></aside><section class="msg-chat" aria-label="信息对话">${empty ? '<div class="msg-empty-state">暂无信息</div>' : `<header class="msg-chat-header" id="msg-chat-header">${headerHTML()}</header><div class="msg-recipient" id="msg-recipient" ${active === "new" ? "" : "hidden"}><label for="msg-to">收件人：</label><input id="msg-to" type="tel" inputmode="tel" placeholder="手机号码" aria-label="收件人手机号" value="${esc(newNumber)}" maxlength="21"></div>${senderHTML()}<div class="msg-transcript scroll-area" id="msg-transcript" role="log" aria-label="聊天内容" aria-live="polite"></div><div class="msg-composer-area"><div class="msg-attachment" id="msg-attachment" hidden></div><form class="msg-composer" id="ipad-message-form"><button type="button" class="msg-attach-button" data-msg-action="attach" aria-label="添加照片"><img src="assets/message-plus.png" alt=""></button><input hidden id="msg-file" type="file" accept="image/png,image/jpeg,image/webp,image/gif"><div class="msg-input-wrap"><textarea id="msg-input" aria-label="信息内容" placeholder="短信" rows="1" maxlength="2000">${esc(drafts[active] || "")}</textarea><button class="msg-send" type="submit" aria-label="发送信息" disabled><img src="assets/message-send.png" alt=""><span class="msg-spinner" aria-hidden="true"></span></button></div></form></div>`}</section></div>`;
  }
  function bubbleHTML(message, previous, next) {
    const date = new Date(message.at);
    const isNewDay = !previous || date.toDateString() !== new Date(previous.at).toDateString();
    const grouped = next && message.mine === next.mine && next.at >= message.at &&
      next.at - message.at < 300000 && date.toDateString() === new Date(next.at).toDateString();
    const portrait = message.mine ? "" : grouped
      ? '<span class="msg-avatar-space" aria-hidden="true"></span>' : avatar(current());
    return `${isNewDay ? `<div class="msg-date">${UI.day(message.at)} ${UI.time(message.at)}</div>` : ""}<div class="msg-line ${message.mine ? "outgoing" : "incoming"}${grouped ? " grouped" : ""}">${portrait}<div class="msg-content"><div class="msg-bubble ${message.image ? "with-image" : ""}">${message.image ? `<img src="${esc(message.image)}" alt="信息中的照片">` : ""}${emptyReceived(message) ? '<span class="msg-placeholder">无文本内容</span>' : message.text ? `<span>${esc(message.text)}</span>` : ""}</div>${deliveryHTML(message)}</div></div>`;
  }
  function scrollToLatest() {
    const node = $("#msg-transcript");
    if (node) node.scrollTop = node.scrollHeight;
  }
  function transcript() {
    const node = $("#msg-transcript");
    if (!node) return;
    const items = current()?.messages || [];
    const nearEnd = node.scrollHeight - node.scrollTop - node.clientHeight < 60;
    if (!node.children) {
      node.innerHTML = items.map((message, index) => bubbleHTML(message, items[index - 1], items[index + 1])).join("");
      return;
    }
    const existing = new Map([...node.children].map(child => [child.dataset.messageId, child]));
    const keep = new Set();
    let cursor = node.firstElementChild;
    items.forEach((message, index) => {
      const id = message.id || String(index), html = bubbleHTML(message, items[index - 1], items[index + 1]);
      let row = existing.get(id);
      if (!row) { row = document.createElement("div"); row.dataset.messageId = id; }
      if (row.messageHTML !== html) { row.innerHTML = html; row.messageHTML = html; }
      if (row !== cursor) node.insertBefore(row, cursor); else cursor = cursor.nextElementSibling;
      keep.add(id);
    });
    for (const [id, row] of existing) if (!keep.has(id)) row.remove();
    if (nearEnd || !existing.size) scrollToLatest();
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
    btn.disabled = sending || (!input.value.trim() && !attachment);
    btn.classList.toggle("is-sending", sending);
    btn.setAttribute("aria-busy", String(sending));
    input.style.height = "auto";
    input.style.height = Math.min(input.scrollHeight, 120) + "px";
  }
  function deliveryHTML(message) {
    if (!message.remote) return "";
    if (!message.mine) {
      const labels = {receiving:"正在接收", download_pending:"彩信待接收", downloading:"正在接收", waiting_network:"等待彩信网络", failed:"彩信接收失败", expired:"彩信已过期", decode_error:"信息解码异常", unsupported_push:"暂不支持的信息类型"};
      return labels[message.state] ? `<small class="msg-delivery${message.state === "failed" ? " failed" : ""}" role="status" title="${esc(message.issue || "")}">${labels[message.state]}</small>` : "";
    }
    const state = MessageIdentity.delivery(message);
    if (!state) return "";
    const labels = {sent: "已发送", delivered: "已送达", failed: "尚未送达", unconfirmed: "结果待确认", waiting: "等待彩信网络"};
    return `<small class="msg-delivery${state === "failed" ? " failed" : ""}" role="status" title="${esc(message.issue || "")}">${state === "pending" ? '<span class="msg-spinner" aria-label="发送中"></span>' : labels[state]}</small>`;
  }
  function syncRemote(records, contacts = []) {
    const old = current(), previous = JSON.stringify(old?.messages || []);
    const resolve = MessageIdentity.resolver([...records, ...contacts]);
    const names = new Map();
    for (const contact of contacts) {
      const key = JSON.stringify([contact.lineId, resolve(contact)]);
      if (!names.has(key) || names.get(key).revision < contact.revision) names.set(key, contact);
    }
    const grouped = new Map(), oldThreads = new Map(threads.map(t => [t.id, t]));
    for (const message of records) {
      if (message.deleted) continue;
      const number = resolve(message), id = MessageIdentity.threadId(message, number);
      let thread = grouped.get(id);
      if (!thread) {
        thread = {id, remote:true, number, senderId:message.senderId, lineId:message.lineId,
          name:names.get(JSON.stringify([message.lineId, number]))?.name || "",
          unread:oldThreads.get(id)?.unread || false, messages:[]};
        grouped.set(id, thread);
      }
      thread.messages.push(message);
    }
    for (const thread of grouped.values()) thread.messages.sort((a,b) => a.at-b.at || a.id.localeCompare(b.id));
    threads = [...threads.filter(t => !t.remote), ...grouped.values()];
    if (old?.remote && !current()) {
      const ids = new Set(old.messages.map(m => m.id));
      const replacement = threads.find(t => t.messages.some(m => ids.has(m.id)));
      if (replacement) {
        if (drafts[active]) drafts[replacement.id] = drafts[active];
        delete drafts[active]; active = replacement.id;
      }
    }
    if (active !== "new" && !current()) {
      const next = threads[0]?.id || "";
      if (active !== next) {
        active = next;
        if ($(".messages-app")) { refresh(); return; }
      }
    }
    const list = $("#msg-thread-list"); if (list) list.innerHTML = listHTML();
    if (old?.name !== current()?.name || old?.number !== current()?.number) {
      const header = $("#msg-chat-header"); if (header) header.innerHTML = headerHTML();
    }
    if (previous !== JSON.stringify(current()?.messages || []) || old?.name !== current()?.name) transcript();
  }
  function unmount() { unsubscribe?.(); unsubscribe = null; }
  function mount() {
    if (typeof MessageData !== "undefined" && !unsubscribe && MessageData.enabled()) {
      unsubscribe = () => {};
      unsubscribe = MessageData.subscribe(syncRemote);
    }
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
  async function send() {
    if (sending) return;
    if (typeof MessageData === "undefined" || !MessageData.enabled()) {toast("信息服务尚未接入");return;}
    const input = $("#msg-input"), text = input.value.trim();
    if (!text && !attachment) return;
    const isNew = active === "new", thread = current();
    const number = isNew ? $("#msg-to").value.replace(/[\s()-]/g, "") : thread?.number;
    if (!/^\+?\d{3,15}$/.test(number || "")) {toast("请输入有效的手机号码");return;}
    const selected = Lines.resolve(thread ? senderId(thread) : defaultSender, attachment ? "mms" : "sms");
    const item = ModuleData.items.find(item => item.id === selected), line = item?.sims.find(sim => sim.enabled);
    if (!item || !line) {toast(attachment ? "暂无彩信配置就绪的模块" : "暂无短信服务就绪的模块");return;}
    if (thread?.lineId && thread.lineId !== line.id) {toast("原 SIM 已切换，请重新选择发件人");return;}
    const payload = {moduleId:item.id,lineId:line.id,to:number,text,image:attachment?.data || ""};
    const signature = JSON.stringify(payload);
    if (!pendingSubmit || pendingSubmit.signature !== signature) pendingSubmit = {signature,requestId:Http.id()};
    sending = true; updateSend();
    const originalActive = active;
    try {
      const result = await MessageData.send({...payload,requestId:pendingSubmit.requestId});
      pendingSubmit = null;
      if (active !== originalActive) return;
      const id = threads.find(t => t.messages.some(m => m.id === result.id))?.id || MessageIdentity.threadId(result);
      active = id; newNumber = ""; delete drafts.new; drafts[id] = ""; attachment = null;
      if (isNew) refresh(); else {input.value="";renderAttachment();transcript();}
    } catch (error) {
      const labels = {INVALID_IMAGE:"请选择不超过 1 MB 的 JPG、PNG 或 GIF 图片",MMS_TOO_LARGE:"图片需小于 1 MB",MESSAGE_RATE_LIMIT:"发送过于频繁，请稍后再试",DEVICE_CHANGED:"SIM 状态已变化，请刷新后重试",REQUEST_CONFLICT:"发送请求冲突，请核实记录"};
      toast(labels[error.code] || "提交结果待确认，再次提交将核对同一请求");
    } finally {sending = false;updateSend();}
  }
  async function removeThreads(id) {
    const remote = threads.filter(t => t.remote && (!id || t.id === id)).flatMap(t => t.messages.map(m => m.id));
    if (remote.length) {try {await MessageData.remove(remote);} catch {toast("删除未完成，请稍后重试");return;}}
    const index = threads.findIndex((thread) => thread.id === id);
    const next = id ? threads.filter((thread) => thread.id !== id) : [];
    if (!UI.write(key, next.filter(t => !t.remote))) return;
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
      if (thread.remote) return [thread];
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
    if (!UI.write(key, next.filter(t => !t.remote))) return false;
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
    if (e.target.closest("[data-msg-note-cancel]")) { $("#dialog").close(); return; }
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
    if (action === "note") editNote();
  });
  document.addEventListener("input", (e) => {
    if (e.target.id === "msg-note-name") $("#msg-note-count").textContent = `${Array.from(e.target.value).length}/24`;
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
    if (e.target.id === "message-note-form") { e.preventDefault(); saveNote(e.target); return; }
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
      !["image/png", "image/jpeg", "image/gif"].includes(
        file.type,
      ) ||
      file.size > 1024 * 1024
    ) {
      toast("请选择 1 MB 以内的图片");
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
  return { render, mount, unmount, prune };
})();
