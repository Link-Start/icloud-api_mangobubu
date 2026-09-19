export const ALIAS_DELETION_POLL_INTERVAL_MS = 2_000;
export const ALIAS_DELETION_REQUEST_TIMEOUT_MS = 10_000;

export const ALIAS_DELETION_OPERATION_LABELS = Object.freeze({
  validate: "校验",
  list: "获取邮箱列表",
  deactivate: "停用邮箱",
  delete: "删除邮箱",
});

export function formatAliasDeletionResultMessage(result) {
  if (result?.deleted === true) return "已删除";
  const retained = result?.localRetained === true;
  const code = typeof result?.code === "string" ? result.code.trim() : "";
  let message = typeof result?.message === "string" ? result.message.trim() : "";
  let retentionNoticeSeen = false;
  // Keep one confirmed notice, and do not repeat an unconfirmed retention claim.
  message = message.replace(/(?:[；;，,]\s*)?本地记录已保留/g, (notice) => {
    if (!retained || retentionNoticeSeen) return "";
    retentionNoticeSeen = true;
    return notice;
  }).replace(/^[；;，,。\s]+|[；;，,\s]+$/g, "");

  if (code === "APPLE_BATCH_DEFERRED") {
    message ||= "主号持续限流，尚未向 Apple 发送请求";
    if (!message.includes("未执行")) message = `未执行：${message}`;
  } else {
    message ||= code && code !== "UNKNOWN" ? code : "删除结果待确认";
  }
  const notice = retained ? "本地记录已保留" : "Apple / 本地状态待核对";
  return message.includes(notice) ? message : `${message}；${notice}`;
}

export function isAliasDeletionJobActive(job) {
  return ["queued", "running", "waiting", "paused"].includes(job?.status);
}

export function isAliasDeletionJobTerminal(job) {
  return ["completed", "interrupted", "cancelled"].includes(job?.status);
}

export function createAliasDeletionOperationId(crypto = globalThis.crypto) {
  if (crypto?.randomUUID) return crypto.randomUUID();
  // getRandomValues also works when randomUUID is absent on HTTP deployments.
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

export function createAliasDeletionStorage(basePath, username, storage) {
  const key = `icloud-api:alias-deletion:v1:${encodeURIComponent(basePath)}:${encodeURIComponent(username)}`;
  function session() {
    return storage === undefined ? globalThis.sessionStorage : storage;
  }
  return {
    read() {
      try {
        const value = JSON.parse(session()?.getItem(key) || "null");
        return value && typeof value.operationId === "string" && value.operationId
          ? { operationId: value.operationId }
          : null;
      } catch {
        return null;
      }
    },
    write(value) {
      try {
        // Store identifiers only, never mailbox contents or credentials.
        session()?.setItem(key, JSON.stringify({
          operationId: value.operationId,
        }));
      } catch {
        // The server's administrator-scoped latest endpoint still supports recovery.
      }
    },
    clear() {
      try {
        session()?.removeItem(key);
      } catch {
        // Storage can be disabled by the browser.
      }
    },
  };
}

function validJob(job, expectedId) {
  if (typeof job?.jobId !== "string" || !job.jobId || (expectedId && job.jobId !== expectedId) ||
      (!isAliasDeletionJobActive(job) && !isAliasDeletionJobTerminal(job)) ||
      ![job.requested, job.processed, job.deleted, job.failed].every(
        (count) => Number.isSafeInteger(count) && count >= 0,
      )) {
    throw Object.assign(new Error("任务查询响应异常，删除结果待确认。"), {
      code: "INVALID_RESPONSE",
    });
  }
  return job;
}

export function aliasDeletionJobLabel(job) {
  if (job?.cancelRequested && isAliasDeletionJobActive(job)) return "正在取消剩余项";
  if (job?.status === "completed" && job.cancelled > 0) return "已结束（含取消项）";
  if (isAliasDeletionJobActive(job)) {
    const accounts = job.accounts || [];
    if (accounts.some((account) => account.status === "running")) return "执行中";
    if (accounts.some((account) => account.status === "paused" && account.waitReason === "login_required")) return "等待主号登录";
    if (accounts.some((account) => account.status === "paused")) return "已暂停，等待恢复";
    if (accounts.some((account) => account.status === "waiting") || job.waits?.length) return "等待后继续";
  }
  return { queued: "排队中", running: "执行中", waiting: "等待后继续", paused: "等待主号登录", completed: "已完成", interrupted: "已中断", cancelled: "已取消" }[job?.status] || "查询中";
}

export function aliasDeletionJobType(job) {
  if (job?.status === "interrupted" || job?.failed > 0) return "warning";
  if (job?.status === "completed") return job.cancelled > 0 ? "info" : "success";
  return "info";
}

export function aliasDeletionPending(job) {
  return Number.isSafeInteger(job?.pending) ? job.pending
    : Math.max(0, (job?.requested || 0) - (job?.processed || 0));
}

export function aliasDeletionPercentage(job) {
  return job?.requested ? Math.min(100, Math.max(0,
    (job.processed || 0) / job.requested * 100)) : 0;
}

export function aliasDeletionWaitLabel(reason) {
  return { quota: "等待每小时额度恢复", upstream_rate_limit: "等待 Apple 恢复请求额度", login_required: "等待主号重新登录 Apple" }[reason] || "等待后继续";
}

export function aliasDeletionAccountLabel(status) {
  return { queued: "排队中", running: "执行中", waiting: "等待后继续", paused: "已暂停", completed: "已完成", cancelled: "已取消", interrupted: "已中断" }[status] || "排队中";
}

/** One list poll at a time; active jobs never occupy the submission slot. */
export function createAliasDeletionController({
  startJob,
  getJob,
  getJobs,
  cancelJob,
  clearCompletedJobs,
  storage,
  onChange = () => {},
  createOperationId = createAliasDeletionOperationId,
  setTimeoutFn = globalThis.setTimeout,
  clearTimeoutFn = globalThis.clearTimeout,
  intervalMs = ALIAS_DELETION_POLL_INTERVAL_MS,
  timeoutMs = ALIAS_DELETION_REQUEST_TIMEOUT_MS,
}) {
  let pending = storage?.read() || null;
  let state = {
    jobs: [],
    operationId: pending?.operationId || "",
    recovering: true,
    submitting: false,
    checking: false,
    uncertain: Boolean(pending),
    unmatched: false,
    error: null,
    blocked: Boolean(pending),
    cancelling: [],
    clearing: false,
  };
  let stopped = false;
  let started = false;
  let timer = null;
  let inFlight = null;
  const readControllers = new Set();
  const revisions = new Map();
  const clearedJobIds = new Set();

  function update(patch) {
    if (stopped) return;
    state = { ...state, ...patch, operationId: pending?.operationId || "" };
    state.blocked = state.submitting || Boolean(pending);
    onChange(state);
  }

  function clearTimer() {
    if (timer !== null) clearTimeoutFn(timer);
    timer = null;
  }

  function clearPending() {
    pending = null;
    storage?.clear();
  }

  function mergeJobs(jobs, expectedRevisions) {
    const merged = new Map(state.jobs.map((job) => [job.jobId, job]));
    for (const job of jobs) {
      if (clearedJobIds.has(job.jobId)) continue;
      if (expectedRevisions && revisions.get(job.jobId) !== expectedRevisions.get(job.jobId)) continue;
      merged.set(job.jobId, job);
      revisions.set(job.jobId, (revisions.get(job.jobId) || 0) + 1);
    }
    let terminalCount = 0;
    const sorted = [...merged.values()].sort((a, b) =>
      Number(isAliasDeletionJobActive(b)) - Number(isAliasDeletionJobActive(a)) ||
      (Date.parse(b.createdAt) || 0) - (Date.parse(a.createdAt) || 0));
    update({ jobs: sorted.filter((job) => isAliasDeletionJobActive(job) || ++terminalCount <= 100) });
  }

  function acceptSubmission(job, operationId) {
    validJob(job, operationId);
    clearPending();
    mergeJobs([job]);
    update({ error: null, uncertain: false, unmatched: false });
  }

  function schedule() {
    clearTimer();
    if (stopped || inFlight || state.submitting || state.clearing) return;
    if (state.recovering || state.uncertain || state.jobs.some(isAliasDeletionJobActive)) {
      timer = setTimeoutFn(() => {
        timer = null;
        void refresh();
      }, intervalMs);
    }
  }

  async function shortRequest(request, { read = false } = {}) {
    const controller = new AbortController();
    if (read) readControllers.add(controller);
    const deadline = setTimeoutFn(() => controller.abort(), timeoutMs);
    try {
      return await request({ signal: controller.signal });
    } finally {
      clearTimeoutFn(deadline);
      readControllers.delete(controller);
    }
  }

  function refresh() {
    if (stopped) return Promise.resolve(false);
    if (inFlight) return inFlight;
    if (state.submitting || state.clearing) return Promise.resolve(false);
    clearTimer();
    update({ checking: true });
    const operationId = pending?.operationId;
    const expectedRevisions = new Map(revisions);
    const request = Promise.resolve().then(async () => {
      if (stopped) return false;
      try {
        // A lost response is reconciled by its exact ID. The list is fetched too,
        // so an unresolved submission cannot hide progress on earlier tasks.
        const requests = [shortRequest(getJobs, { read: true })];
        if (operationId) requests.push(shortRequest((options) => getJob(operationId, options), { read: true }));
        const [listed, confirmed] = await Promise.allSettled(requests);
        if (stopped) return false;
        let listError = null;
        let confirmationRevision = expectedRevisions.get(operationId);
        try {
          if (listed.status === "rejected") throw listed.reason;
          if (!Array.isArray(listed.value)) throw Object.assign(new Error("任务列表响应异常。"), { code: "INVALID_RESPONSE" });
          listed.value.forEach((job) => validJob(job));
          const listedIds = new Set(listed.value.map((job) => job.jobId));
          const missingActive = state.jobs.filter((job) => isAliasDeletionJobActive(job) &&
            expectedRevisions.has(job.jobId) && revisions.get(job.jobId) === expectedRevisions.get(job.jobId) &&
            !listedIds.has(job.jobId));
          mergeJobs(listed.value, expectedRevisions);
          confirmationRevision = revisions.get(operationId);
          // The list includes every active job, but only recent terminal jobs.
          // Resolve older jobs individually when many complete between polls.
          const recovered = await Promise.allSettled(missingActive.map((job) =>
            shortRequest((options) => getJob(job.jobId, options), { read: true })));
          if (stopped) return false;
          for (let index = 0; index < recovered.length; index++) {
            const result = recovered[index];
            if (result.status === "fulfilled") {
              try { mergeJobs([validJob(result.value, missingActive[index].jobId)], expectedRevisions); }
              catch (error) { listError ||= error; }
            } else { listError ||= result.reason; }
          }
        } catch (error) { listError = error; }

        // A new submit can start while this list read is pending. Never clear
        // that newer operation's evidence or replace it with list/latest data.
        if (operationId && pending?.operationId === operationId) {
          try {
            if (confirmed.status === "rejected") throw confirmed.reason;
            validJob(confirmed.value, operationId);
            if (revisions.get(operationId) === confirmationRevision) {
              acceptSubmission(confirmed.value, operationId);
            } else {
              // Cancellation may finish while missing-history lookups are in
              // flight. Confirmation resolves submission evidence, not that
              // newer job result.
              clearPending();
              update({ error: null, uncertain: false, unmatched: false });
            }
          } catch (error) {
            update({ error, recovering: false, uncertain: true,
              unmatched: error?.status === 404 && error?.code === "NOT_FOUND" });
            return false;
          }
        }
        if (!pending) update({ error: listError, uncertain: Boolean(listError), unmatched: false });
        update({ recovering: Boolean(listError) });
        return !listError;
      } catch (error) {
        update({ error, recovering: true, uncertain: true });
        return false;
      } finally {
        update({ checking: false });
      }
    }).finally(() => {
      if (inFlight === request) inFlight = null;
      schedule();
    });
    inFlight = request;
    return request;
  }

  return {
    getState: () => state,
    start() {
      if (started || stopped) return inFlight || Promise.resolve(false);
      started = true;
      return refresh();
    },
    refresh,
    async submit(ids, csrfToken) {
      if (stopped || state.blocked || !ids.length) return false;
      const operationId = createOperationId();
      pending = { operationId };
      storage?.write(pending);
      clearTimer();
      update({ submitting: true, error: null, unmatched: false, uncertain: false });
      try {
        const job = await shortRequest(
          (options) => startJob([...ids], operationId, csrfToken, options),
        );
        if (stopped) return false;
        acceptSubmission(job, operationId);
      } catch (error) {
        if (stopped) return false;
        if ((error?.status >= 400 && error.status < 500 && error.status !== 408 && error.code !== "INVALID_RESPONSE") ||
                   (error?.status === 503 && error.code === "BATCH_DELETE_UNAVAILABLE")) {
          clearPending();
          throw error;
        } else {
          update({ uncertain: true, error });
        }
      } finally {
        update({ submitting: false });
        if (!stopped && pending) void refresh();
        else schedule();
      }
      return true;
    },
    // Explicit reconciliation only; it clears selection in the view and never retries DELETE.
    acknowledgeUnmatched() {
      if (stopped || !state.unmatched || state.checking) return false;
      clearPending();
      clearTimer();
      update({ error: null, uncertain: false, unmatched: false, recovering: true });
      void refresh();
      return true;
    },
    async cancel(jobId, csrfToken) {
      const job = state.jobs.find((item) => item.jobId === jobId);
      if (stopped || !cancelJob || !isAliasDeletionJobActive(job) || job.cancelRequested || state.cancelling.includes(jobId)) return false;
      update({ cancelling: [...state.cancelling, jobId] });
      try {
        const cancelled = await shortRequest((options) => cancelJob(jobId, csrfToken, options));
        if (stopped) return false;
        mergeJobs([validJob(cancelled, jobId)]);
        return true;
      } finally {
        update({ cancelling: state.cancelling.filter((id) => id !== jobId) });
        if (!stopped) void refresh();
      }
    },
    async clearCompleted(csrfToken) {
      if (stopped || !clearCompletedJobs || state.clearing || state.recovering || state.blocked ||
          !state.jobs.some((job) => job.status === "completed")) return false;
      clearTimer();
      update({ clearing: true });
      try {
        const result = await shortRequest((options) => clearCompletedJobs(csrfToken, options));
        if (stopped) return false;
        // Keep late list, lookup and cancellation replies from restoring cleared history.
        for (const id of result.clearedJobIds) clearedJobIds.add(id);
        update({ jobs: state.jobs.filter((job) => !clearedJobIds.has(job.jobId)) });
        return result;
      } finally {
        update({ clearing: false });
        schedule();
      }
    },
    stop() {
      stopped = true;
      clearTimer();
      for (const controller of readControllers) controller.abort();
    },
  };
}
