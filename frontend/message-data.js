const MessageData = (() => {
  const summaries = new Map(), contacts = new Map(), listeners = new Set(), windows = new Map();
  let history = null, historyRequest = null;
  let historyDirty = false, search = { text:"", items:[] }, searchTimer = null, searchRequest = null;
  let cursor = 0, contactCursor = 0, snapshot = 0, timer = null, controller = null, users = 0;
  const enabled = () => Backend.enabled("messages");
  const identity = item => JSON.stringify([item.senderId, item.lineId, item.number]);
  const imageURL = item => ({ ...item, image: item.image ? Http.apiURL(`/messages/${encodeURIComponent(item.id)}/image`) : null, remote: true });
  function merge(item, provisional = false) {
    if (!item || typeof item.id !== "string" || !Number.isSafeInteger(item.revision)) return false;
    const key = identity(item);
    const previous = summaries.get(key);
    if ((previous?.revision || 0) > item.revision || previous?.revision === item.revision && !(previous.provisional && !provisional)) return false;
    if (provisional && previous && previous.at > item.at) item = { ...previous, revision:item.revision };
    for (const [key, cached] of windows) if (matches(item, cached.scope)) windows.delete(key);
    summaries.set(key, { ...imageURL(item), provisional });
    return true;
  }
  function mergeContact(item) {
    if (!item || typeof item.lineId !== "string" || typeof item.number !== "string" || !Number.isSafeInteger(item.revision)) return false;
    const key = JSON.stringify([item.lineId, item.number]);
    if ((contacts.get(key)?.revision || 0) >= item.revision) return false;
    contacts.set(key, item);
    return true;
  }
  const notify = () => { for (const listener of listeners) listener([...summaries.values()].filter(item => !item.deleted), [...contacts.values()], { paged: true, history, search }); };
  function stop() {
    clearTimeout(timer); timer = null; controller?.abort(); controller = null;
    historyRequest?.abort(); historyRequest = null;
    clearTimeout(searchTimer); searchTimer = null; searchRequest?.abort(); searchRequest = null;
    if (history) history.busy = false;
  }
  const matches = (item, scope) => item.senderId === scope.moduleId && item.lineId === scope.lineId && scope.numbers.includes(item.number);
  async function loadWindow(scope, direction = "", anchor = "") {
    historyRequest?.abort();
    historyDirty = false;
    const request = new AbortController(); historyRequest = request;
    const key = JSON.stringify(scope);
    history = { ...(history?.key === key ? history : { items: [], earlier: false, newer: false }), key, scope, busy: true, error: "" };
    notify();
    try {
      const page = await Backend.messages.window({ query: { ...scope, numbers: JSON.stringify(scope.numbers), direction, anchor }, signal: request.signal });
      if (request.signal.aborted || historyRequest !== request) return;
      history = { key, scope, items: (page.items || []).map(imageURL), earlier: page.earlier === true, newer: page.newer === true, pinned: direction !== "", busy: false, error: "" };
      if (!history.pinned && !historyDirty) {
        windows.delete(key);
        windows.set(key, { ...history, loadedAt: Date.now() });
        while (windows.size > 12) windows.delete(windows.keys().next().value);
      }
    } catch (error) {
      if (request.signal.aborted || historyRequest !== request) return;
      history.busy = false; history.error = error.code === "HISTORY_EXPIRED" ? "这段记录已清理，请查看最新信息" : "信息读取失败，请重试";
    } finally {
      if (historyRequest === request) {
        historyRequest = null; notify();
        if (historyDirty) refreshWindow();
      }
    }
  }
  function refreshWindow() {
    if (!history) return;
    if (historyRequest) { historyDirty = true; return; }
    const anchor = history.pinned ? history.items.at(-1)?.id : "";
    return loadWindow(history.scope, anchor ? "current" : "", anchor || "");
  }
  async function poll() {
    if (!users || document.hidden || !enabled() || controller) return;
    clearTimeout(timer); timer = null;
    const request = new AbortController(); controller = request;
    let more = false;
    try {
      const data = await Backend.messages.threads({ query: { after: cursor, contactAfter: contactCursor, ...(snapshot ? { snapshot } : {}) }, signal: request.signal });
      if (request.signal.aborted) return;
      let changed = false, refresh = false;
      for (const item of data.contacts || []) changed = mergeContact(item) || changed;
      for (const item of data.items || []) {
        if (merge(item)) { changed = true; refresh ||= !!history && matches(item, history.scope); }
      }
      if (Number.isSafeInteger(data.contactCursor) && data.contactCursor >= contactCursor) contactCursor = data.contactCursor;
      if (Number.isSafeInteger(data.cursor) && data.cursor >= cursor) cursor = data.cursor;
      more = data.more === true;
      snapshot = more && Number.isSafeInteger(data.snapshot) ? data.snapshot : 0;
      if (changed) notify();
      if (refresh) await refreshWindow();
    } catch (error) {
      if (error.code === "CURSOR_EXPIRED") {
        cursor = 0; contactCursor = 0; snapshot = 0; summaries.clear(); contacts.clear(); windows.clear(); more = true; notify();
      } else if (!["ABORTED", "UNAUTHENTICATED"].includes(error.code)) {
        const status = document.querySelector("[data-message-sync]");
        if (status) status.textContent = "信息同步暂不可用";
      }
    } finally {
      if (controller === request) {
        controller = null;
        if (users && !document.hidden) timer = setTimeout(poll, more ? 25 : 3000);
      }
    }
  }
  document.addEventListener("visibilitychange", () => { stop(); if (!document.hidden) { poll(); refreshWindow(); if (search.text) MessageData.search(search.text); } });
  window.addEventListener("pagehide", stop);
  return {
    enabled,
    search(text) {
      clearTimeout(searchTimer); searchRequest?.abort(); searchRequest = null;
      search = { text, items:[], busy:!!text }; notify();
      if (!text) return;
      searchTimer = setTimeout(async () => {
        const request = new AbortController(); searchRequest = request;
        try {
          const result = await Backend.messages.search({ query:{text}, signal:request.signal });
          if (!request.signal.aborted) search = { text, items:result.items || [], more:result.more === true };
        } catch (error) {
          if (!request.signal.aborted) search = { text, items:[], error:"搜索暂不可用，请重试" };
        } finally {
          if (searchRequest === request) { searchRequest = null; notify(); }
        }
      }, 250);
    },
    watch(thread) {
      if (!thread?.remotePaged) { historyRequest?.abort(); historyRequest = null; history = null; return; }
      const scope = { moduleId: thread.senderId, lineId: thread.lineId, numbers: [...thread.numbers].sort() };
      const key = JSON.stringify(scope);
      if (history?.key === key) return;
      historyRequest?.abort(); historyRequest = null; historyDirty = false;
      const cached = windows.get(key);
      if (cached) {
        history = { ...cached };
        windows.delete(key); windows.set(key, cached);
        notify();
        if (Date.now() - cached.loadedAt < 30000) return;
      }
      return loadWindow(scope);
    },
    page(action) {
      if (!history || history.busy) return;
      const direction = action === "latest" ? "" : action;
      const anchor = direction === "earlier" ? history.items[0]?.id : direction === "newer" ? history.items.at(-1)?.id : "";
      return loadWindow(history.scope, direction, anchor || "");
    },
    async saveContact(body) {
      const item = await Backend.messages.saveContact({ body });
      if (mergeContact(item)) notify();
      return item;
    },
    async send(body) {
      const item = await Backend.messages.send({ body });
      if (merge(item, true)) notify();
      if (history && matches(item, history.scope)) await refreshWindow();
      return item;
    },
    async removeThreads(scope) {
      let result = { snapshot: 0 };
      do { result = await Backend.messages.removeThreads({ body: { ...scope, snapshot: result.snapshot } }); } while (result.more);
      stop(); cursor = 0; contactCursor = 0; snapshot = 0; summaries.clear(); contacts.clear(); windows.clear(); history = null; notify();
      await poll();
    },
    subscribe(listener) {
      listeners.add(listener); users++; listener([...summaries.values()].filter(item => !item.deleted), [...contacts.values()], { paged: true, history, search }); poll();
      if (users === 1 && search.text) MessageData.search(search.text);
      return () => { if (listeners.delete(listener)) users--; if (!users) stop(); };
    },
  };
})();
