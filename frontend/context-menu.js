// 列表共用右键菜单与删除确认，不修改页面数据。
const ContextMenu = (() => {
  const { $, escape } = UI;
  const menu = $("#context-menu"),
    dialog = $("#dialog");
  let items = [],
    opener = null,
    pending = null;
  function close(restoreFocus = false) {
    menu.hidden = true;
    items = [];
    if (restoreFocus && opener?.isConnected) opener.focus();
    opener = null;
  }
  function open(event, entries) {
    event.preventDefault();
    close();
    items = entries;
    opener = event.target.closest("button, [tabindex]");
    menu.innerHTML = entries
      .map(
        (item, index) =>
          `<button type="button" role="menuitem" class="${item.danger ? "danger" : ""}" data-menu-item="${index}" ${item.disabled ? "disabled" : ""}>${escape(item.label)}</button>`,
      )
      .join("");
    menu.hidden = false;
    const anchor = event.target.getBoundingClientRect();
    const x = event.clientX || anchor.left,
      y = event.clientY || anchor.bottom;
    menu.style.left = `${Math.max(8, Math.min(x, innerWidth - menu.offsetWidth - 8))}px`;
    menu.style.top = `${Math.max(8, Math.min(y, innerHeight - menu.offsetHeight - 8))}px`;
    menu.querySelector("button:not(:disabled)")?.focus();
  }
  function confirm(title, action, label = "删除") {
    close();
    pending = action;
    UI.modal(
      title,
      `<div class="confirm-actions"><button type="button" data-confirm="cancel">取消</button><button type="button" class="danger-button" data-confirm="accept">${escape(label)}</button></div>`,
    );
    $('[data-confirm="cancel"]').focus();
  }
  document.addEventListener("click", (event) => {
    const option = event.target.closest("[data-menu-item]");
    if (option && !menu.hidden) {
      const item = items[Number(option.dataset.menuItem)];
      close();
      if (item && !item.disabled) item.action();
      return;
    }
    const confirmation = event.target.closest("[data-confirm]");
    if (!confirmation) return;
    const action = confirmation.dataset.confirm === "accept" ? pending : null;
    pending = null;
    dialog.close();
    action?.();
  });
  document.addEventListener("pointerdown", (event) => {
    if (!menu.hidden && !menu.contains(event.target)) close();
  });
  document.addEventListener("contextmenu", (event) => {
    if (menu.contains(event.target)) event.preventDefault();
    else close();
  });
  document.addEventListener("keydown", (event) => {
    if (menu.hidden) return;
    if (["Escape", "Tab"].includes(event.key)) {
      if (event.key === "Escape") event.preventDefault();
      close(true);
    } else if (["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) {
      event.preventDefault();
      const buttons = [...menu.querySelectorAll("button:not(:disabled)")];
      const index = buttons.indexOf(document.activeElement);
      const next =
        event.key === "Home"
          ? 0
          : event.key === "End"
            ? buttons.length - 1
            : (index + (event.key === "ArrowDown" ? 1 : -1) + buttons.length) %
              buttons.length;
      buttons[next]?.focus();
    }
    event.stopImmediatePropagation();
  });
  dialog.addEventListener("close", () => {
    pending = null;
  });
  document.addEventListener("scroll", () => close(), true);
  window.addEventListener("resize", () => close());
  window.addEventListener("blur", () => close());
  return { open, close, confirm };
})();
