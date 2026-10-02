// 电话与短信共用选择浮层，沿用原有 change 保存逻辑。
(() => {
  let active = null;
  const esc = UI.escape;
  function close(restoreFocus = false) {
    if (!active) return;
    const { trigger, panel } = active;
    active = null;
    if (panel.matches(":popover-open")) panel.hidePopover();
    trigger.setAttribute("aria-expanded", "false");
    if (restoreFocus && trigger.isConnected) trigger.focus();
  }
  function position() {
    if (!active) return;
    const { trigger, panel } = active;
    if (!trigger.isConnected || trigger.disabled) return close();
    const r = trigger.getBoundingClientRect();
    const below = window.innerHeight - r.bottom - 20;
    const above = r.top - 20;
    const down = below >= Math.min(370, above);
    panel.style.maxHeight = `${Math.max(120, down ? below : above)}px`;
    panel.style.left = `${Math.max(12, Math.min(r.left + (r.width - panel.offsetWidth) / 2, window.innerWidth - panel.offsetWidth - 12))}px`;
    panel.style.top = `${Math.max(12, down ? r.bottom + 8 : r.top - panel.offsetHeight - 8)}px`;
  }
  function draw(query = "") {
    const { trigger, panel } = active;
    const items = Lines.options(query);
    panel.querySelector(".line-options").innerHTML = items.length
      ? items
          .map(
            (item) =>
              `<button type="button" class="line-option" role="option" tabindex="-1" data-line-value="${esc(item.id)}" aria-selected="${item.id === trigger.value}"><span class="line-option-copy"><span>${esc(item.label)}</span>${item.number ? `<small>${esc(item.number)}</small>` : ""}</span><svg viewBox="0 0 20 20" aria-hidden="true"><path d="m4 10 4 4 8-8"/></svg></button>`,
          )
          .join("")
      : '<p class="line-empty" role="status">未找到模块</p>';
    panel.querySelector(".line-options").scrollTop = 0;
    position();
  }
  function open(trigger) {
    if (trigger.disabled) return;
    if (active?.trigger === trigger) return close(true);
    close();
    const panel = document.getElementById(
      trigger.getAttribute("aria-controls"),
    );
    const searchId = `${trigger.id}-search`;
    panel.innerHTML = `<div class="line-search"><svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="8.5" cy="8.5" r="5.5"/><path d="m13 13 4 4"/></svg><input id="${esc(searchId)}" type="search" autocomplete="off" spellcheck="false" placeholder="搜索模块或号码" aria-label="搜索模块或号码" aria-controls="${esc(trigger.id)}-options"></div><div class="line-options" id="${esc(trigger.id)}-options" role="listbox" aria-label="模块"></div>`;
    active = { trigger, panel };
    draw();
    panel.showPopover();
    trigger.setAttribute("aria-expanded", "true");
    position();
    panel.querySelector("input").focus({ preventScroll: true });
  }
  function choose(value) {
    if (!active || active.trigger.disabled || !Lines.valid(value)) return;
    const { trigger } = active;
    trigger.value = value;
    trigger.dispatchEvent(new Event("change", { bubbles: true }));
    // 保存失败时，业务监听器会恢复原值。
    trigger.querySelector("span").textContent = Lines.caption(trigger.value);
    close(true);
  }
  document.addEventListener("click", (event) => {
    const trigger = event.target.closest(".line-trigger");
    if (trigger) return open(trigger);
    if (!active?.panel.contains(event.target)) return;
    const option = event.target.closest("[data-line-value]");
    if (option) choose(option.dataset.lineValue);
  });
  document.addEventListener("input", (event) => {
    if (
      active?.panel.contains(event.target) &&
      event.target.matches('input[type="search"]')
    )
      draw(event.target.value);
  });
  document.addEventListener("keydown", (event) => {
    const trigger = event.target.closest(".line-trigger");
    if (trigger && ["ArrowDown", "ArrowUp"].includes(event.key)) {
      event.preventDefault();
      if (!active) open(trigger);
      return;
    }
    if (!active?.panel.contains(event.target) || event.isComposing) return;
    if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      return close(true);
    }
    if (event.key === "Tab") return close(true);
    const options = [...active.panel.querySelectorAll("[data-line-value]")];
    const index = options.indexOf(event.target);
    if (event.key === "Enter" && index === -1) {
      event.preventDefault();
      if (options.length) choose(options[0].dataset.lineValue);
      return;
    }
    let next;
    if (event.key === "ArrowDown")
      next = Math.min(index + 1, options.length - 1);
    else if (event.key === "ArrowUp") next = index <= 0 ? -1 : index - 1;
    else if (index >= 0 && event.key === "Home") next = 0;
    else if (index >= 0 && event.key === "End") next = options.length - 1;
    else return;
    event.preventDefault();
    if (next < 0) active.panel.querySelector("input").focus();
    else {
      options[next].focus({ preventScroll: true });
      options[next].scrollIntoView({ block: "nearest" });
    }
  });
  document.addEventListener(
    "toggle",
    (event) => {
      if (
        active &&
        event.target === active.panel &&
        !active.panel.matches(":popover-open")
      )
        close();
    },
    true,
  );
  document.addEventListener(
    "scroll",
    (event) => {
      if (active && !active.panel.contains(event.target)) position();
    },
    true,
  );
  window.addEventListener("resize", position);
})();
