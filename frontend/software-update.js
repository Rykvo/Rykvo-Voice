const SoftwareUpdate = (() => {
  const errors = {
    UPDATE_SIGNATURE_REQUIRED: "请选择加密更新包", UPDATE_SIGNATURE_INVALID: "更新包签名无效",
    UPDATE_KEY_UNAVAILABLE: "升级密钥未就绪，请联系管理员", UPDATE_DECRYPT_FAILED: "更新包解密失败",
    UPDATE_CHUNK_INCOMPLETE: "上传中断，重新选择文件可继续",
    INVALID_UPDATE_PACKAGE: "更新包无效或不完整", UPDATE_DOWNGRADE: "更新版本低于当前版本",
    UPDATE_STORAGE_LOW: "主机空间不足", UPDATE_BUSY: "已有更新正在处理",
    UPDATE_CONFLICT: "更新包已变化，请重新选择", UPDATE_UNAVAILABLE: "更新服务暂未就绪",
    UPDATE_INSTALL_FAILED: "更新未完成，原有服务已尝试恢复", UPDATE_INTERRUPTED: "更新中断，请检查主机状态",
    SERVICE_DRAINING: "主机正在更新", UNAUTHENTICATED: "请重新登录",
    HTTP_413: "更新包过大", TIMEOUT: "上传超时，请重试", NETWORK: "连接中断，重新选择文件可继续",
  };
  const pageVersion = document.currentScript?.getAttribute("src")?.match(/[?&]v=([^&]+)/)?.[1] || "";
  let closeActive = () => {}, needsReload = false;
  function open() {
    closeActive();
    modal("软件更新", `<section class="software-update"><dl class="general-details"><div><dt>当前版本</dt><dd data-update-version>—</dd></div></dl><div class="update-upload-row"><span>上传更新</span><label class="primary update-file"><input type="file" accept=".rvu" aria-label="上传更新包"><span>上传</span></label></div><p class="update-note" data-update-file></p><progress max="100" value="0" aria-label="上传进度" hidden></progress><p class="update-note" data-update-status role="status" aria-live="polite"></p><div class="update-actions" hidden><button type="button" class="text-button update-cancel" data-update-cancel hidden>取消</button><button type="button" class="primary" data-update-start disabled hidden>更新</button><button type="button" class="primary" data-update-reload hidden>重新载入</button></div></section>`);
    const root = document.querySelector(".software-update"), dialog = document.querySelector("#dialog");
    const file = root.querySelector("input"), start = root.querySelector("[data-update-start]"), info = root.querySelector("[data-update-status]"), progress = root.querySelector("progress");
    const controller = new AbortController();
    let timer, ticket = "", targetVersion = "", busy = false, generation = 0, pollingSince = 0, confirming = false, completedVersion = "";
    const active = () => dialog.open && root.isConnected && !controller.signal.aborted;
    const text = error => errors[error.code] || "操作未完成，请重试";
    const stop = () => { clearTimeout(timer); controller.abort(); dialog.removeEventListener("close", stop); };
    closeActive = stop;
    dialog.addEventListener("close", stop, { once: true });
    function show(data) {
      if (!active()) return;
      confirming = false;
      root.querySelector("[data-update-cancel]").hidden = true;
      start.textContent = "更新";
      busy = ["validating", "queued", "running"].includes(data.state);
      if (["queued", "running"].includes(data.state)) needsReload = true;
      const complete = data.state === "complete" && (needsReload || !pageVersion || (data.version || "").localeCompare(pageVersion, "en", { numeric: true }) > 0);
      if (["idle", "complete", "failed"].includes(data.state)) root.querySelector("[data-update-file]").textContent = "";
      if (data.state === "complete" && data.version && needsReload) root.querySelector("[data-update-version]").textContent = completedVersion = data.version;
      ticket = data.state === "ready" ? data.ticket : "";
      targetVersion = data.version || "";
      file.disabled = busy; start.disabled = !ticket; start.hidden = !ticket;
      root.querySelector("[data-update-reload]").hidden = !complete;
      root.querySelector(".update-actions").hidden = !ticket && !complete;
      const labels = { uploading: "重新选择同一文件可继续上传", validating: "正在校验…", idle: "", ready: "等待确认 · " + targetVersion, queued: "等待更新，请勿关闭主机", running: "正在更新，请勿关闭主机", complete: complete ? "更新完成 · " + targetVersion : "", failed: errors[data.error] || "更新未完成，请重试" };
      info.textContent = labels[data.state] || "";
      if (busy) { pollingSince ||= Date.now(); schedule(); }
      else { clearTimeout(timer); pollingSince = 0; }
    }
    function schedule() { clearTimeout(timer); if (active()) timer = setTimeout(poll, document.hidden ? 10000 : 2000); }
    async function poll() {
      if (!active()) return;
      const turn = generation;
      try { const data = await Backend.softwareUpdate.get({ signal: controller.signal }); if (turn === generation) show(data); }
      catch (error) {
        if (!active() || turn !== generation) return;
        if (busy && Date.now() - pollingSince < 2100000) { info.textContent = "正在重新连接…"; schedule(); }
        else info.textContent = text(error);
      }
    }
    Backend.version.get({ signal: controller.signal }).then(data => { if (active() && !completedVersion) root.querySelector("[data-update-version]").textContent = data.version === "development" ? "开发版本" : data.version || "—"; }).catch(() => { if (active() && !completedVersion) root.querySelector("[data-update-version]").textContent = "读取失败"; });
    poll();
    file.addEventListener("change", async () => {
      const selected = file.files[0]; if (!selected || file.disabled) return;
      generation++; clearTimeout(timer); ticket = ""; start.disabled = true; confirming = false;
      root.querySelector("[data-update-cancel]").hidden = true; start.textContent = "更新"; start.hidden = true;
      root.querySelector("[data-update-reload]").hidden = true;
      root.querySelector(".update-actions").hidden = true;
      root.querySelector("[data-update-file]").textContent = "";
      if (!selected.name.toLowerCase().endsWith(".rvu") || selected.size < 141 || selected.size > 130 * 1024 * 1024) { file.value = ""; info.textContent = "请选择加密更新包"; return; }
      file.disabled = true; progress.hidden = false; progress.value = 0; info.textContent = "正在上传 · 0%";
      root.querySelector("[data-update-file]").textContent = selected.name;
      try {
        const data = await UpdateTransfer.upload(selected, { signal: controller.signal, onRetry: () => { if (active()) info.textContent = "连接中断，正在重试…"; }, onProgress: value => { if (active()) { progress.value = value; info.textContent = value === 100 ? "正在校验…" : "正在上传 · " + value + "%"; } } });
        show(data);
      } catch (error) { if (active()) info.textContent = text(error); }
      finally { if (active()) { file.value = ""; file.disabled = busy; progress.hidden = true; } }
    });
    start.addEventListener("click", async () => {
      if (!ticket || busy) return;
      if (!confirming) {
        confirming = true;
        start.textContent = "确认更新";
        root.querySelector("[data-update-cancel]").hidden = false;
        info.textContent = "更新将短暂重启服务";
        return;
      }
      root.querySelector("[data-update-cancel]").hidden = true;
      generation++; start.disabled = true; file.disabled = true; busy = true; needsReload = true; pollingSince = Date.now(); info.textContent = "正在提交…";
      try { show(await Backend.softwareUpdate.apply({ body: { ticket }, signal: controller.signal })); }
      catch (error) { if (active()) { info.textContent = text(error); schedule(); } }
    });
    root.querySelector("[data-update-cancel]").addEventListener("click", () => {
      confirming = false; start.textContent = "更新";
      root.querySelector("[data-update-cancel]").hidden = true;
      info.textContent = "等待确认 · " + targetVersion;
    });
    root.querySelector("[data-update-reload]").addEventListener("click", () => location.reload());
  }
  return { open };
})();
