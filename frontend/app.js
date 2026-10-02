const { $, modal } = UI;
const defaults = { motion: false, theme: "light" };
const saved = UI.read("rykvo-voice-v1", {});
const state = Object.fromEntries(
  Object.entries(defaults).map(([key, value]) => [
    key,
    typeof saved[key] === typeof value ? saved[key] : value,
  ]),
);
let currentPage = "";
function applyPreferences() {
  document.body.classList.toggle("theme-dark", state.theme === "dark");
  document.body.classList.toggle("reduce-motion", state.motion);
}
const { icon } = Forms;
const generalItems = [
  { id: "administrator", title: "管理员", icon: "privacy.svg" },
  { id: "server", title: "服务器", icon: "cloud.svg" },
  { id: "developer", title: "开发者", icon: "developer.svg" },
  { id: "networks", title: "网络配置", icon: "ethernet.svg" },
  { id: "cleanup", title: "主机设置", icon: "settings.png" },
  { id: "updates", title: "软件更新", icon: "updates.svg" },
];
const generalPages = new Set(["administrator", "developer", "developerDocs", "server", "sipServer", "cleanup", "networks"]);
function general() {
  return `<div class="page-head"><h1>通用</h1></div><div class="settings-list general-list">${generalItems.map((item) => `<button type="button" class="setting-row general-entry" data-action="${item.id}" ${generalPages.has(item.id) ? "" : 'aria-haspopup="dialog"'}>${icon(item.icon)}<span class="row-copy"><strong>${item.title}</strong></span><span class="chevron" aria-hidden="true">›</span></button>`).join("")}</div>`;
}
const pages = {
  modules: Modules,
  messages: Messages,
  general: { render: general },
  administrator: Admin,
  developer: Developer,
  developerDocs: DeveloperDocs,
  server: Server,
  sipServer: SIPServer,
  sip: SIP,
  cleanup: HostSettings,
  networks: NetworkSettings,
};
const pageKey = "rykvo-voice-page-v1";
function initialPage() {
  let page = location.hash.slice(1);
  if (!page) {
    try {
      page = sessionStorage.getItem(pageKey);
    } catch {}
  }
  if (page === "phone") page = "sip";
  return Object.hasOwn(pages, page) ? page : "modules";
}
function render(page) {
  ContextMenu.close();
  pages[currentPage]?.unmount?.(page);
  if (!["developer", "developerDocs"].includes(page)) Developer.discardDraft?.();
  currentPage = page;
  $("#app-window main").dataset.page = page;
  $("#page-content").innerHTML =
    `<div class="page-enter">${pages[page].render()}</div>`;
  document.querySelectorAll("[data-page].nav-item").forEach((button) => {
    const active =
      button.dataset.page === (generalPages.has(page) ? "general" : page);
    button.classList.toggle("active", active);
    if (active) button.setAttribute("aria-current", "page");
    else button.removeAttribute("aria-current");
  });
  $(".main-scroll").scrollTop = 0;
  pages[page].mount?.();
  Visibility.apply();
  try {
    sessionStorage.setItem(pageKey, page);
  } catch {}
  history.replaceState(null, "", location.pathname + location.search);
}
const actions = {
  logout: (button) => Auth.logout(button),
  administrator: () => render("administrator"),
  "general-back": () => render("general"),
  server: () => render("server"),
  sipServer: () => render("sipServer"),
  developer: () => render("developer"),
  developerDocs: () => render("developerDocs"),
  cleanup: () => render("cleanup"),
  networks: () => render("networks"),
  updates: () => SoftwareUpdate.open(),
};
document.addEventListener("click", (event) => {
  if (!Auth.authenticated) return;
  const nav = event.target.closest(".nav-item[data-page]");
  if (nav) {
    if (nav.dataset.page !== currentPage) render(nav.dataset.page);
    return;
  }
  const button = event.target.closest("[data-action]");
  if (button) actions[button.dataset.action]?.(button);
});
$("#dialog").addEventListener("click", (event) => {
  if ($(".visibility-settings, .visibility-verify")) return;
  if ($("#network-edit")) return;
  if (event.target !== event.currentTarget) return;
  const box = event.target.getBoundingClientRect();
  if (
    event.clientX < box.left ||
    event.clientX > box.right ||
    event.clientY < box.top ||
    event.clientY > box.bottom
  )
    event.target.close();
});
applyPreferences();
Auth.start(() => {
  ModuleData.start();
  SIP.preload();
  Visibility.init(generalItems, () => currentPage);
  render(initialPage());
});
