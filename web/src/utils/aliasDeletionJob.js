export const ALIAS_DELETION_POLL_INTERVAL_MS = 2_000;
export const ALIAS_DELETION_REQUEST_TIMEOUT_MS = 10_000;

export function isAliasDeletionJobActive(job) {
  return job?.status === "queued" || job?.status === "running";
}

export function isAliasDeletionJobTerminal(job) {
  return job?.status === "completed" || job?.status === "interrupted";
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

/** A single request at a time; errors retry reads, never replay the mutation. */
export function createAliasDeletionController({
  startJob,
  getJob,
  getLatestJob,
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
    job: null,
    operationId: pending?.operationId || "",
    recovering: true,
    submitting: false,
    checking: false,
    uncertain: Boolean(pending),
    unmatched: false,
    error: null,
    blocked: true,
  };
  let stopped = false;
  let started = false;
  let timer = null;
  let inFlight = null;
  let readController = null;

  function update(patch) {
    if (stopped) return;
    state = { ...state, ...patch, operationId: pending?.operationId || "" };
    state.blocked = state.recovering || state.submitting || state.checking ||
      state.uncertain || isAliasDeletionJobActive(state.job);
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

  function accept(job) {
    if (job) validJob(job);
    if (isAliasDeletionJobTerminal(job)) {
      clearPending();
    } else if (job) {
      pending = { operationId: job.jobId };
      storage?.write(pending);
    }
    update({ job, error: null, uncertain: false, unmatched: false, recovering: false });
  }

  function schedule() {
    clearTimer();
    if (stopped || inFlight || state.submitting) return;
    if (state.recovering || state.uncertain || isAliasDeletionJobActive(state.job)) {
      timer = setTimeoutFn(() => {
        timer = null;
        void refresh();
      }, intervalMs);
    }
  }

  async function shortRequest(request, { read = false } = {}) {
    const controller = new AbortController();
    if (read) readController = controller;
    const deadline = setTimeoutFn(() => controller.abort(), timeoutMs);
    try {
      return await request({ signal: controller.signal });
    } finally {
      clearTimeoutFn(deadline);
      if (readController === controller) readController = null;
    }
  }

  function refresh({ latest = false } = {}) {
    if (stopped) return Promise.resolve(false);
    if (inFlight) return inFlight;
    if (state.submitting) return Promise.resolve(false);
    clearTimer();
    update({ checking: true });
    // The operation ID IS the job ID, even when the DELETE response was lost.
    // Never let latest (including a manual refresh) replace unresolved evidence.
    const jobId = pending?.operationId || (!latest && !state.recovering && state.job?.jobId);
    const request = Promise.resolve().then(async () => {
      try {
        if (stopped) return false;
        const job = await shortRequest(
          (options) => jobId ? getJob(jobId, options) : getLatestJob(options),
          { read: true },
        );
        if (stopped) return false;
        if (job || jobId) validJob(job, jobId);
        accept(job);
        return true;
      } catch (error) {
        update({
          error,
          recovering: !jobId,
          uncertain: true,
          unmatched: Boolean(jobId && error?.status === 404 && error?.code === "NOT_FOUND"),
        });
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
      return refresh({ latest: true });
    },
    refresh,
    async submit(ids, csrfToken) {
      if (stopped || state.blocked || inFlight || !ids.length) return false;
      const operationId = createOperationId();
      pending = { operationId };
      storage?.write(pending);
      clearTimer();
      update({ job: null, submitting: true, error: null, unmatched: false });
      try {
        const job = await shortRequest(
          (options) => startJob([...ids], operationId, csrfToken, options),
        );
        if (stopped) return false;
        accept(validJob(job, operationId));
      } catch (error) {
        if (stopped) return false;
        if (error?.status === 409 && error?.code === "BATCH_DELETE_IN_PROGRESS") {
          clearPending();
          update({ job: null, recovering: true, uncertain: true, error });
        } else if ((error?.status >= 400 && error.status < 500 && error.status !== 408 && error.code !== "INVALID_RESPONSE") ||
                   (error?.status === 503 && error.code === "BATCH_DELETE_UNAVAILABLE")) {
          clearPending();
          throw error;
        } else {
          update({ uncertain: true, error });
        }
      } finally {
        update({ submitting: false });
        if (!stopped && (state.uncertain || state.recovering)) void refresh({ latest: true });
        else schedule();
      }
      return true;
    },
    // Explicit reconciliation only; it clears selection in the view and never retries DELETE.
    acknowledgeUnmatched() {
      if (stopped || !state.unmatched || state.checking) return false;
      clearPending();
      clearTimer();
      update({ job: null, error: null, uncertain: false, unmatched: false, recovering: true });
      void refresh({ latest: true });
      return true;
    },
    stop() {
      stopped = true;
      clearTimer();
      readController?.abort();
    },
  };
}
