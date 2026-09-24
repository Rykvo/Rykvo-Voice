// 电话和新短信使用同一份模块数据。
const Lines = (() => {
  const valid = (id) =>
    id === "random" || ModuleData.items.some((item) => item.id === id);
  const recorded = (id) =>
    id !== "random" && (valid(id) || /^module-\d+$/.test(id));
  const available = (item) =>
    (!item.managed || item.capabilities?.calls === true) &&
    item.status === "online" && item.signal !== "none" && item.sims[0]?.enabled;
  function resolve(id) {
    if (id !== "random")
      return ModuleData.items.find((item) => item.id === id && available(item))
        ?.id;
    const candidates = ModuleData.items.filter(available);
    return candidates[Math.floor(Math.random() * candidates.length)]?.id;
  }
  function options(query = "") {
    const normalize = (value) => value.toLowerCase().replace(/[\s·+()-]/g, "");
    const needle = normalize(query.trim());
    return [
      { id: "random", label: "随机", number: "" },
      ...ModuleData.items.map((item) => ({
        id: item.id,
        label: item.label || item.name,
        name: item.name,
        number: Countries.format(item.number),
      })),
    ].filter(
      (item) =>
        !needle ||
        [item.label, item.name || "", item.number].some((value) =>
          normalize(value).includes(needle),
        ),
    );
  }
  function caption(value) {
    const item = options().find((item) => item.id === value) || options()[0];
    return item.number ? `${item.label} · ${item.number}` : item.label;
  }
  function select(id, label, value) {
    const selected = valid(value) ? value : "random";
    return `<button type="button" class="line-trigger" id="${UI.escape(id)}" value="${UI.escape(selected)}" aria-label="${UI.escape(label)}" aria-haspopup="dialog" aria-expanded="false" aria-controls="${UI.escape(id)}-picker"><span>${UI.escape(caption(selected))}</span><svg viewBox="0 0 16 16" aria-hidden="true"><path d="m5 6 3 3 3-3"/></svg></button><div class="line-picker" id="${UI.escape(id)}-picker" popover="auto" role="dialog" aria-label="${UI.escape(label)}"></div>`;
  }
  return { valid, recorded, resolve, options, caption, select };
})();
