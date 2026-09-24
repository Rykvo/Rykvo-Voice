const UI = (() => {
  const $ = (selector, root = document) => root.querySelector(selector);
  const escape = (value) =>
    String(value).replace(
      /[&<>"']/g,
      (char) =>
        ({
          "&": "&amp;",
          "<": "&lt;",
          ">": "&gt;",
          '"': "&quot;",
          "'": "&#39;",
        })[char],
    );
  let toastTimer;
  const id = Http.id;
  function toast(text) {
    clearTimeout(toastTimer);
    const notice = $("#toast");
    notice.textContent = text;
    // 顶层提示不被原生对话框的背景遮挡。
    notice.hidePopover?.();
    notice.showPopover?.();
    notice.classList.add("show");
    toastTimer = setTimeout(() => {
      notice.classList.remove("show");
      notice.hidePopover?.();
    }, 2500);
  }
  function read(key, fallback) {
    try {
      return JSON.parse(localStorage.getItem(key)) ?? fallback;
    } catch {
      return fallback;
    }
  }
  function write(key, value) {
    try {
      localStorage.setItem(key, JSON.stringify(value));
      return true;
    } catch {
      toast("内容暂未保存，请检查浏览器储存空间");
      return false;
    }
  }
  function modal(title, body) {
    $("#dialog-content").innerHTML =
      `<h2 id="dialog-title">${escape(title)}</h2>${body}`;
    $("#dialog").showModal();
  }
  async function copy(text) {
    try {
      await navigator.clipboard.writeText(text);
      toast("号码已复制");
    } catch {
      modal(
        "复制号码",
        `<input class="copy-number" aria-label="待复制号码" value="${escape(text)}" readonly>`,
      );
      $(".copy-number").select();
    }
  }
  const duration = (seconds) =>
    `${String(Math.floor(seconds / 60)).padStart(2, "0")}:${String(seconds % 60).padStart(2, "0")}`;
  const time = (value) =>
    new Date(value).toLocaleTimeString("zh-CN", {
      hour: "2-digit",
      minute: "2-digit",
      hour12: false,
    });
  function day(value) {
    const date = new Date(value),
      today = new Date(),
      yesterday = new Date();
    yesterday.setDate(today.getDate() - 1);
    if (date.toDateString() === today.toDateString()) return "今天";
    if (date.toDateString() === yesterday.toDateString()) return "昨天";
    return date.toLocaleDateString("zh-CN", { month: "long", day: "numeric" });
  }
  // 拨号字符可重复，按光标位置编辑。
  function editDial(value, key, start = value.length, end = start) {
    if (key === "clear") return { value: "", caret: 0 };
    if (key === "backspace" || key === "forward-delete") {
      if (start === end) {
        if (key === "backspace") start = Math.max(0, start - 1);
        else end = Math.min(value.length, end + 1);
      }
      return { value: value.slice(0, start) + value.slice(end), caret: start };
    }
    if (key === "delete")
      return {
        value: value.slice(0, -1),
        caret: Math.max(0, value.length - 1),
      };
    if (!/^[0-9+*#]$/.test(key)) return { value, caret: start };
    return {
      value: value.slice(0, start) + key + value.slice(end),
      caret: start + 1,
    };
  }
  return {
    $,
    id,
    escape,
    toast,
    read,
    write,
    modal,
    copy,
    duration,
    time,
    day,
    editDial,
  };
})();
