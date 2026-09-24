const ServerNavigation = (() => {
  const tabs = [["server", "主机服务器"], ["sipServer", "SIP 电话服务器"]];
  function header(active) {
    return `${Forms.header("服务器", "cloud.svg")}<nav class="server-tabs" aria-label="服务器类型">${tabs.map(([id, label]) => `<button type="button" data-action="${id}" aria-pressed="${id === active}">${label}</button>`).join("")}</nav>`;
  }
  return { header };
})();
