const ServerNavigation = (() => {
  const tabs = [["server", "主机服务器"], ["sipServer", "SIP 电话服务器"]];
  function header(active) {
    return `${Forms.header("服务器", "cloud.svg")}${Forms.tabs("服务器类型", tabs, active)}`;
  }
  return { header };
})();
