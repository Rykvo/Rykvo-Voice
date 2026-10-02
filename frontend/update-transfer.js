const UpdateTransfer = (() => {
  const hex = bytes => Array.from(new Uint8Array(bytes), value => value.toString(16).padStart(2, "0")).join("");
  const transient = error => ["NETWORK", "TIMEOUT", "UPDATE_BUSY", "UPDATE_UNAVAILABLE", "UPDATE_CHUNK_INCOMPLETE", "HTTP_408", "HTTP_429", "HTTP_502", "HTTP_503", "HTTP_504", "HTTP_520", "HTTP_524"].includes(error.code);
  function pause(ms, signal) {
    return new Promise((resolve, reject) => {
      if (signal?.aborted) { reject(Http.failure("ABORTED", "已取消")); return; }
      const done = () => { signal?.removeEventListener("abort", abort); resolve(); };
      const timer = setTimeout(done, ms);
      const abort = () => { clearTimeout(timer); signal.removeEventListener("abort", abort); reject(Http.failure("ABORTED", "已取消")); };
      signal?.addEventListener("abort", abort, { once: true });
    });
  }
  async function upload(file, { signal, onProgress, onRetry } = {}) {
    if (!file.name.toLowerCase().endsWith(".rvu") || file.size < 141 || file.size > 130 * 1024 * 1024)
      throw Http.failure("UPDATE_SIGNATURE_REQUIRED", "请选择加密更新包");
    const header = hex(await file.slice(0, 28).arrayBuffer());
    const seal = hex(await file.slice(file.size - 96).arrayBuffer());
    const retry = async action => {
      for (let attempt = 0; ; attempt++) {
        if (signal?.aborted) throw Http.failure("ABORTED", "已取消");
        try { return await action(); }
        catch (error) {
          if (!transient(error) || attempt === 3) throw error;
          onRetry?.(attempt + 1);
          await pause(1000 * 2 ** attempt, signal);
        }
      }
    };
    const post = (action, body) => Http.request("POST", "/software-update/upload/" + action, { body, signal });
    let state = await retry(() => post("start", { size: file.size, header, seal }));
    if (["ready", "validating"].includes(state.state)) return state;
    if (state.state !== "uploading" || !/^[a-f0-9]{32}$/.test(state.uploadId) || !Number.isInteger(state.offset) || state.offset < 0 || state.offset > file.size || !Number.isInteger(state.chunkSize) || state.chunkSize < 1 || state.chunkSize > 1024 * 1024)
      throw Http.failure("INVALID_RESPONSE", "上传状态异常");
    const id = state.uploadId, size = state.chunkSize;
    onProgress?.(Math.floor(state.offset / file.size * 100));
    while (state.offset < file.size) {
      const offset = state.offset, end = Math.min(offset + size, file.size);
      const next = await retry(() => Http.upload("/software-update/upload/chunk", file.slice(offset, end), {
        signal, headers: { "Upload-ID": id, "Upload-Offset": String(offset) },
      }));
      if (next.state !== "uploading" || next.uploadId !== id || next.offset !== end)
        throw Http.failure("UPDATE_CONFLICT", "上传状态已变化");
      state = next;
      // Browser send progress can include proxy buffers; only server acknowledgements count.
      onProgress?.(Math.floor(state.offset / file.size * 100));
    }
    return retry(() => post("finish", { uploadId: id }));
  }
  return { upload };
})();
