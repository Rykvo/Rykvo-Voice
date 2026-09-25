const MessageData = (() => {
  const records = new Map(), listeners = new Set();
  let contacts = [];
  let cursor = 0, timer = null, controller = null, users = 0;
  const enabled = () => Backend.enabled("messages");
  function merge(item) {
    if (!item || typeof item.id !== "string" || !Number.isSafeInteger(item.revision)) return false;
    if ((records.get(item.id)?.revision || 0) >= item.revision) return false;
    records.set(item.id, { ...item, image: item.image ? Http.apiURL(`/messages/${encodeURIComponent(item.id)}/image`) : null, remote: true });
    return true;
  }
  function mergeContact(item) {
    if (!item || typeof item.lineId !== "string" || typeof item.number !== "string" || !Number.isSafeInteger(item.revision)) return false;
    const index = contacts.findIndex(c => c.lineId === item.lineId && c.number === item.number);
    if (index >= 0 && contacts[index].revision >= item.revision) return false;
    if (index < 0) contacts.push(item); else contacts[index] = item;
    return true;
  }
  const notify = () => { for (const listener of listeners) listener([...records.values()], contacts); };
  function stop() { clearTimeout(timer); timer = null; controller?.abort(); controller = null; }
  async function poll() {
    if (!users || document.hidden || !enabled() || controller) return;
    const request = new AbortController(); controller = request;
    let more = false;
    try {
      const data = await Backend.messages.list({ query: { after: cursor }, signal: request.signal });
      if (request.signal.aborted) return;
      let changed = false;
      for (const item of data.contacts || []) changed = mergeContact(item) || changed;
      for (const item of data.items || []) changed = merge(item) || changed;
      if (Number.isSafeInteger(data.cursor) && data.cursor >= cursor) cursor = data.cursor;
      more = data.more === true;
      if (changed) notify();
    } catch (error) {
      if (!["ABORTED", "UNAUTHENTICATED"].includes(error.code)) {
        const status = document.querySelector("[data-message-sync]");
        if (status) status.textContent = "信息同步暂不可用";
      }
    } finally {
      if (controller === request) {
        controller = null;
        if (users && !document.hidden) timer = setTimeout(poll, more ? 0 : 3000);
      }
    }
  }
  document.addEventListener("visibilitychange", () => { stop(); if (!document.hidden) poll(); });
  window.addEventListener("pagehide", stop);
  return {
    enabled,
    async saveContact(body) {
      const item = await Backend.messages.saveContact({ body });
      if (mergeContact(item)) notify();
      return item;
    },
    async send(body) { const item = await Backend.messages.send({ body }); if (merge(item)) notify(); return item; },
    async remove(ids) { for (const id of ids) { await Backend.messages.remove({params:{messageId:id}}); records.delete(id); } notify(); },
    subscribe(listener) { listeners.add(listener); users++; listener([...records.values()], contacts); poll(); return () => { if (listeners.delete(listener)) users--; if (!users) stop(); }; },
  };
})();
