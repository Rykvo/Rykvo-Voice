// 只依据同一 SIM 已出现的国际号码合并，不猜测本地号码的国家。
const MessageIdentity = (() => {
  const clean = value => String(value || "").trim().replace(/[\s().-]/g, "");
  const scope = item => item.lineId || item.senderId || "local";
  function resolver(items) {
    const aliases = new Map();
    for (const item of items) {
      const number = clean(item.number).replace(/^00/, "+");
      const code = Countries.code(number);
      if (!code || !/^\+\d{7,15}$/.test(number)) continue;
      const national = number.slice(code.length + 1);
      const variants = [number.slice(1)];
      if (national.length >= 7) variants.push(national);
      if (code === "44" && national.length === 10) variants.push("0" + national);
      for (const variant of variants) {
        const key = JSON.stringify([scope(item), variant]);
        if (!aliases.has(key)) aliases.set(key, new Set());
        aliases.get(key).add(number);
      }
    }
    return item => {
      const raw = clean(item.number);
      if (!/^\+?\d+$/.test(raw)) return String(item.number || "").trim();
      const number = raw.replace(/^00/, "+");
      if (number.startsWith("+")) return number;
      const matches = aliases.get(JSON.stringify([scope(item), number]));
      return matches?.size === 1 ? [...matches][0] : number;
    };
  }
  const threadId = (item, number = item.number) => `remote:${item.senderId}:${item.lineId}:${number}`;
  function avatar(number, name = "") {
    let hash = 2166136261;
    for (const char of number) hash = Math.imul(hash ^ char.codePointAt(0), 16777619);
    const colors = ["blue", "green", "purple", "orange", "pink", "teal"];
    const text = name.trim() ? Array.from(name.trim()).slice(0, 2).join("")
      : /^\+?\d+$/.test(clean(number)) ? clean(number).slice(-2)
      : Array.from(number.replace(/^[@#]/, "")).slice(0, 2).join("").toUpperCase();
    return { text, color: colors[(hash >>> 0) % colors.length] };
  }
  function delivery(message) {
    if (!message.mine) return "";
    if (message.state === "waiting_network" && message.kind === "mms") return "failed";
    if (["queued", "sending", "waiting_network"].includes(message.state)) return "pending";
    if (message.state === "delivered") return "delivered";
    return "failed";
  }
  return { clean, scope, resolver, threadId, avatar, delivery };
})();
