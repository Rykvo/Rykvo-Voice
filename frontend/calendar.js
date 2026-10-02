const Calendar = (() => {
  const esc = UI.escape;
  let active = null;
  function dateKey(value = Date.now()) {
    const d = new Date(value);
    if (!Number.isFinite(d.getTime())) return "";
    return `${String(d.getFullYear()).padStart(4, "0")}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
  }
  function parse(value) {
    if (!/^\d{4}-\d{2}-\d{2}$/.test(value)) return null;
    const d = new Date(`${value}T12:00:00`);
    return dateKey(d) === value ? d : null;
  }
  function shiftMonth(value, step) {
    const d = parse(value);
    if (!d) return "";
    const day = d.getDate();
    d.setDate(1);
    d.setMonth(d.getMonth() + step);
    const last = new Date(d.getFullYear(), d.getMonth() + 1, 0).getDate();
    d.setDate(Math.min(day, last));
    return dateKey(d);
  }
  function monthDays(value) {
    const d = parse(value);
    if (!d) return [];
    d.setDate(1);
    d.setDate(1 - ((d.getDay() + 6) % 7));
    return Array.from({ length: 42 }, () => {
      const key = dateKey(d);
      d.setDate(d.getDate() + 1);
      return key;
    });
  }
  const icon =
    '<svg viewBox="0 0 20 20" aria-hidden="true"><rect x="3" y="4" width="14" height="13" rx="3"/><path d="M6 2.5v3M14 2.5v3M3 8h14M7 11h1M12 11h1M7 14h1"/></svg>';
  function render(id, value = dateKey()) {
    const date = parse(value) ? value : dateKey();
    return `<div class="date-picker"><input type="hidden" id="${esc(id)}" value="${date}"><button type="button" class="date-trigger" data-calendar-open="${esc(id)}" aria-label="选择日期，${date}" aria-haspopup="dialog" aria-expanded="false" aria-controls="${esc(id)}-calendar"><span>${date.replaceAll("-", "/")}</span>${icon}</button><div class="date-calendar" id="${esc(id)}-calendar" popover="auto" role="dialog" aria-label="选择日期"></div></div>`;
  }
  function position() {
    if (!active || !active.panel.matches(":popover-open")) return;
    const { panel, trigger } = active;
    const r = trigger.getBoundingClientRect();
    const width = panel.offsetWidth,
      height = panel.offsetHeight;
    const left = Math.max(
      12,
      Math.min(r.right - width, window.innerWidth - width - 12),
    );
    let top = r.bottom + 8;
    if (top + height > window.innerHeight - 12) top = r.top - height - 8;
    panel.style.left = `${left}px`;
    panel.style.top = `${Math.max(12, top)}px`;
  }
  function draw(focus = false) {
    const { panel, month, cursor, input } = active;
    const d = parse(month),
      today = dateKey();
    const days = monthDays(month);
    const cells = days.map((key) => {
      const day = parse(key);
      const selected = key === input.value;
      return `<div role="gridcell" aria-selected="${selected}"><button type="button" class="calendar-day" data-calendar-day="${key}" data-outside="${day.getMonth() !== d.getMonth()}" ${key === today ? 'aria-current="date"' : ""} tabindex="${key === cursor ? 0 : -1}" aria-label="${day.getFullYear()}年${day.getMonth() + 1}月${day.getDate()}日">${day.getDate()}</button></div>`;
    });
    panel.innerHTML = `<div class="calendar-heading"><strong aria-live="polite">${d.getFullYear()}年${d.getMonth() + 1}月</strong><div><button type="button" class="calendar-nav" data-calendar-month="-1" aria-label="上个月"><svg viewBox="0 0 20 20" aria-hidden="true"><path d="m12 5-5 5 5 5"/></svg></button><button type="button" class="calendar-nav" data-calendar-month="1" aria-label="下个月"><svg viewBox="0 0 20 20" aria-hidden="true"><path d="m8 5 5 5-5 5"/></svg></button></div></div><div class="calendar-week" aria-hidden="true">${["一", "二", "三", "四", "五", "六", "日"].map((day) => `<span>${day}</span>`).join("")}</div><div class="calendar-grid" role="grid" aria-label="${d.getFullYear()}年${d.getMonth() + 1}月">${Array.from({ length: 6 }, (_, i) => `<div role="row">${cells.slice(i * 7, i * 7 + 7).join("")}</div>`).join("")}</div><div class="calendar-footer"><button type="button" data-calendar-today>今天</button></div>`;
    position();
    if (focus) panel.querySelector('[tabindex="0"]').focus();
  }
  function choose(value) {
    if (!active || !parse(value)) return;
    const { input, trigger, panel } = active;
    input.value = value;
    trigger.querySelector("span").textContent = value.replaceAll("-", "/");
    trigger.setAttribute("aria-label", `选择日期，${value}`);
    input.dispatchEvent(new Event("change", { bubbles: true }));
    panel.hidePopover();
    trigger.setAttribute("aria-expanded", "false");
    active = null;
    trigger.focus();
  }
  document.addEventListener("click", (event) => {
    const opener = event.target.closest("[data-calendar-open]");
    if (opener) {
      const input = document.getElementById(opener.dataset.calendarOpen);
      const panel = document.getElementById(`${input.id}-calendar`);
      if (panel.matches(":popover-open")) {
        panel.hidePopover();
        return;
      }
      if (active?.panel.matches(":popover-open")) active.panel.hidePopover();
      const value = parse(input.value) ? input.value : dateKey();
      active = { input, trigger: opener, panel, month: value, cursor: value };
      draw();
      panel.showPopover();
      opener.setAttribute("aria-expanded", "true");
      position();
      panel.querySelector('[tabindex="0"]').focus();
      return;
    }
    if (!active || !active.panel.contains(event.target)) return;
    const day = event.target.closest("[data-calendar-day]");
    if (day) return choose(day.dataset.calendarDay);
    if (event.target.closest("[data-calendar-today]")) return choose(dateKey());
    const nav = event.target.closest("[data-calendar-month]");
    if (nav) {
      const step = Number(nav.dataset.calendarMonth);
      const next = shiftMonth(active.month, step);
      if (!parse(next) || next < "0001-01-01" || next > "9999-12-31") return;
      active.month = next;
      active.cursor = next;
      draw();
      active.panel.querySelector(`[data-calendar-month="${step}"]`).focus();
    }
  });
  document.addEventListener("keydown", (event) => {
    if (!active || !active.panel.contains(event.target)) return;
    if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      const { panel, trigger } = active;
      panel.hidePopover();
      trigger.setAttribute("aria-expanded", "false");
      active = null;
      trigger.focus();
      return;
    }
    const button = event.target.closest("[data-calendar-day]");
    if (!button) return;
    const d = parse(button.dataset.calendarDay);
    const offset = { ArrowLeft: -1, ArrowRight: 1, ArrowUp: -7, ArrowDown: 7 };
    if (Object.hasOwn(offset, event.key))
      d.setDate(d.getDate() + offset[event.key]);
    else if (event.key === "Home")
      d.setDate(d.getDate() - ((d.getDay() + 6) % 7));
    else if (event.key === "End")
      d.setDate(d.getDate() + 6 - ((d.getDay() + 6) % 7));
    else if (event.key === "PageUp" || event.key === "PageDown")
      d.setTime(
        parse(
          shiftMonth(
            button.dataset.calendarDay,
            event.key === "PageUp" ? -1 : 1,
          ),
        ).getTime(),
      );
    else return;
    event.preventDefault();
    const next = dateKey(d);
    if (!parse(next) || next < "0001-01-01" || next > "9999-12-31") return;
    active.cursor = next;
    active.month = next;
    draw(true);
  });
  document.addEventListener(
    "toggle",
    (event) => {
      if (
        active &&
        event.target === active.panel &&
        !active.panel.matches(":popover-open")
      ) {
        active.trigger.setAttribute("aria-expanded", "false");
        active = null;
      }
    },
    true,
  );
  document.addEventListener("scroll", position, true);
  window.addEventListener("resize", position);
  return { render, dateKey, parse, monthDays, shiftMonth };
})();
