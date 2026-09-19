import assert from "node:assert/strict";
import test from "node:test";
import {
  ALIAS_DELETION_POLL_INTERVAL_MS, ALIAS_DELETION_REQUEST_TIMEOUT_MS,
  createAliasDeletionController, createAliasDeletionOperationId, createAliasDeletionStorage,
  formatAliasDeletionResultMessage, isAliasDeletionJobActive, isAliasDeletionJobTerminal,
} from "../src/utils/aliasDeletionJob.js";
const operationId = "63c21a27-6ef4-425f-a11c-edda65c29267";
const job = (status = "running", patch = {}) => ({
  jobId: operationId, status, requested: 7, processed: 0, deleted: 0, failed: 0,
  pending: 7, cancelled: 0, cancelRequested: false, accounts: [], waits: [], results: [], ...patch,
});
const apiError = (status, code) => Object.assign(new Error(code), { status, code });
const flushPromises = () => new Promise((resolve) => setImmediate(resolve));
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
function memoryStorage() {
  const values = new Map();
  return { values, getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value), removeItem: (key) => values.delete(key) };
}
function fakeTimers() {
  let nextId = 0;
  const timers = new Map();
  return {
    setTimeoutFn(callback, delay) { const id = ++nextId; timers.set(id, { callback, delay }); return id; },
    clearTimeoutFn: (id) => timers.delete(id),
    fireNext() {
      const [id, timer] = timers.entries().next().value || [];
      assert.ok(timer, "expected a scheduled timer");
      timers.delete(id); timer.callback(); return timer.delay;
    },
    delays: () => [...timers.values()].map(({ delay }) => delay),
  };
}
function harness(overrides = {}) {
  const timers = fakeTimers();
  const memory = memoryStorage();
  const storage = createAliasDeletionStorage("/install/admin", "owner", memory);
  const calls = [], changes = [], serverJobs = [];
  let sequence = 0;
  const controller = createAliasDeletionController({
    storage,
    createOperationId: () => sequence++ === 0 ? operationId : "next-operation-" + sequence,
    startJob: async (ids, id, csrf, options) => {
      calls.push(["DELETE", ids, id, csrf, options]);
      const created = job("queued", { jobId: id }); serverJobs.push(created); return created;
    },
    getJob: async (id, options) => { calls.push(["GET", id, options]); return job("running", { jobId: id }); },
    getJobs: async (options) => { calls.push(["list", options]); return [...serverJobs]; },
    cancelJob: async (id, csrf, options) => {
      calls.push(["cancel", id, csrf, options]);
      const snapshot = job("completed", { jobId: id, processed: 7, cancelled: 7, pending: 0, cancelRequested: true });
      const index = serverJobs.findIndex((item) => item.jobId === id);
      serverJobs.splice(index, 1, snapshot); return snapshot;
    },
    onChange: (state) => changes.push(state), ...timers, ...overrides,
  });
  return { controller, timers, storage, memory, calls, changes, serverJobs };
}

test("operation IDs are UUIDs including the HTTP-compatible random-values fallback", () => {
  assert.equal(createAliasDeletionOperationId({ randomUUID: () => operationId }), operationId);
  assert.match(createAliasDeletionOperationId({ getRandomValues: (bytes) => bytes.fill(255) }),
    /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
});

test("jobs omitted by the terminal history limit are resolved individually", async () => {
  let complete = false;
  const active = Array.from({ length: 25 }, (_, index) => job("running", { jobId: `history-job-${index}` }));
  const finished = (value) => ({ ...value, status: "completed", processed: 7, deleted: 7, pending: 0 });
  const queries = [];
  const { controller, timers } = harness({
    getJobs: async () => complete ? active.slice(5).map(finished) : active,
    getJob: async (id) => { queries.push(id); return finished(active.find((value) => value.jobId === id)); },
  });
  await controller.start();
  complete = true;
  await controller.refresh();
  assert.deepEqual(queries, active.slice(0, 5).map((value) => value.jobId));
  assert.equal(controller.getState().jobs.length, 25);
  assert.equal(controller.getState().jobs.filter(isAliasDeletionJobActive).length, 0);
  assert.deepEqual(timers.delays(), []);
  controller.stop();
});

test("pending confirmation preserves cancellation during missing-history lookups", async () => {
  let complete = false;
  const old = Array.from({ length: 25 }, (_, index) => job("running", { jobId: `old-job-${index}` }));
  const reads = new Map(old.slice(0, 5).map((value) => [value.jobId, deferred()]));
  const finished = (value) => ({ ...value, status: "completed", processed: 7, deleted: 7, pending: 0 });
  const { controller } = harness({
    getJobs: async () => complete ? [...old.slice(5).map(finished), job()] : old,
    getJob: async (id) => id === operationId ? job() : reads.get(id).promise,
    startJob: async () => { throw new Error("lost response"); },
    cancelJob: async (id) => job("completed", { jobId: id, processed: 7, cancelled: 7, pending: 0, cancelRequested: true }),
  });
  await controller.start();
  complete = true;
  await controller.submit([91], "csrf");
  await flushPromises();
  await controller.cancel(operationId, "csrf");
  const polling = controller.refresh();
  for (const value of old.slice(0, 5)) reads.get(value.jobId).resolve(finished(value));
  await polling;
  const cancelled = controller.getState().jobs.find((value) => value.jobId === operationId);
  assert.equal(cancelled.status, "completed");
  assert.equal(cancelled.cancelled, 7);
  assert.equal(controller.getState().blocked, false);
  controller.stop();
});

test("storage is isolated by installation and username and writes only the operation ID", () => {
  const memory = memoryStorage();
  const first = createAliasDeletionStorage("/one/admin", "owner", memory);
  const otherUser = createAliasDeletionStorage("/one/admin", "another", memory);
  const otherInstall = createAliasDeletionStorage("/two/admin", "owner", memory);
  first.write({ operationId, jobId: "old-job", alias_ids: [91], csrfToken: "secret", results: [{ address: "private" }] });
  assert.deepEqual([...memory.values.values()].map(JSON.parse), [{ operationId }]);
  assert.deepEqual(first.read(), { operationId });
  assert.equal(otherUser.read(), null);
  assert.equal(otherInstall.read(), null);
  otherUser.write({ operationId: "other-id" });
  first.clear();
  assert.deepEqual(otherUser.read(), { operationId: "other-id" });
  const key = [...memory.values.keys()][0];
  memory.setItem(key, "{broken");
  assert.equal(otherUser.read(), null);
  const disabled = createAliasDeletionStorage("/admin", "owner", {
    getItem() { throw new Error("disabled"); },
    setItem() { throw new Error("disabled"); },
    removeItem() { throw new Error("disabled"); },
  });
  assert.equal(disabled.read(), null);
  assert.doesNotThrow(() => { disabled.write({ operationId }); disabled.clear(); });
});

test("result messages deduplicate retention notices and distinguish deferred and unknown outcomes", () => {
  const rateLimited = { deleted: false, code: "APPLE_RATE_LIMITED", localRetained: true };
  for (const message of [
    "Apple 限流", "Apple 限流；本地记录已保留", "Apple 限流，本地记录已保留。",
    "Apple 限流；本地记录已保留；本地记录已保留",
  ]) {
    const formatted = formatAliasDeletionResultMessage({ ...rateLimited, message });
    assert.equal(formatted.match(/本地记录已保留/g).length, 1);
    assert.match(formatted, /Apple 限流/);
  }
  const completeMessage = "Apple 请求受限，本地记录已保留。请稍后核对。";
  assert.equal(formatAliasDeletionResultMessage({ ...rateLimited, message: completeMessage }), completeMessage);
  for (const message of [undefined, "主号持续限流", "未执行；本地记录已保留"]) {
    const formatted = formatAliasDeletionResultMessage({ ...rateLimited, code: "APPLE_BATCH_DEFERRED", message });
    assert.match(formatted, /^未执行/);
    assert.doesNotMatch(formatted, /失败|APPLE_BATCH_DEFERRED/);
    assert.equal(formatted.match(/本地记录已保留/g).length, 1);
  }
  for (const result of [null, {}, { code: "UNKNOWN" }, { code: "UNKNOWN", message: { raw_body: "secret" } }]) {
    assert.equal(formatAliasDeletionResultMessage(result), "删除结果待确认；Apple / 本地状态待核对");
  }
  for (const localRetained of [false, undefined, "true"]) {
    assert.equal(formatAliasDeletionResultMessage({ ...rateLimited, localRetained, message: "Apple 限流；本地记录已保留" }),
      "Apple 限流；Apple / 本地状态待核对");
  }
  assert.equal(formatAliasDeletionResultMessage({ message: "Apple / 本地状态待核对" }), "Apple / 本地状态待核对");
  assert.equal(formatAliasDeletionResultMessage({ deleted: true, message: "本地记录已保留" }), "已删除");
});


test("empty recovery permits submission and all administrator tasks are restored", async () => {
  const snapshots = [job(), job("queued", { jobId: "second" }), job("completed", { jobId: "older" })];
  const { controller, storage, calls, timers } = harness({ getJobs: async () => snapshots });
  assert.equal(controller.getState().blocked, false);
  await controller.start();
  assert.deepEqual(controller.getState().jobs, snapshots);
  assert.equal(controller.getState().blocked, false);
  assert.equal(storage.read(), null);
  assert.equal(await controller.start(), false);
  assert.deepEqual(timers.delays(), [ALIAS_DELETION_POLL_INTERVAL_MS]);
  assert.deepEqual(calls, []);
  controller.stop();
});

test("one uncertain submit is stored before mutation; confirmed active jobs allow more submissions", async () => {
  const response = deferred();
  const { controller, storage, timers } = harness({
    startJob: (ids, id, csrf, { signal }) => {
      assert.deepEqual(storage.read(), { operationId });
      assert.deepEqual(ids, [91, 92]); assert.equal(id, operationId); assert.equal(csrf, "csrf");
      assert.equal(signal.aborted, false); return response.promise;
    },
  });
  await controller.start();
  const ids = [91, 92];
  const submitted = controller.submit(ids, "csrf"); ids.push(93);
  assert.equal(controller.getState().blocked, true);
  assert.equal(await controller.submit([94], "csrf"), false);
  assert.equal(await controller.refresh(), false);
  assert.deepEqual(timers.delays(), [ALIAS_DELETION_REQUEST_TIMEOUT_MS]);
  response.resolve(job("queued")); await submitted;
  assert.equal(controller.getState().blocked, false);
  assert.equal(storage.read(), null);
  assert.deepEqual(timers.delays(), [2_000]);
  controller.stop();
});

test("independent tasks and same-account appends remain visible without an execution cap", async () => {
  const { controller, calls } = harness();
  await controller.start();
  for (let index = 0; index < 8; index += 1) {
    assert.equal(await controller.submit([100 + index], "csrf"), true);
    assert.equal(controller.getState().blocked, false);
  }
  await controller.refresh();
  assert.equal(controller.getState().jobs.length, 8);
  assert.equal(new Set(controller.getState().jobs.map((item) => item.jobId)).size, 8);
  assert.equal(calls.filter(([method]) => method === "DELETE").length, 8);
  assert.equal(calls.filter(([method]) => method === "GET").length, 0);
  controller.stop();
});

test("list polling is serial and continues until every active task finishes", async () => {
  const response = deferred();
  let reads = 0;
  const first = job("running", { requested: 1000, processed: 200, deleted: 200, pending: 800 });
  const second = job("running", { jobId: "second" });
  const { controller, timers } = harness({
    getJobs: async () => ++reads === 1 ? [first, second] : response.promise,
  });
  await controller.start();
  assert.equal(timers.fireNext(), 2_000); await flushPromises();
  const refresh = controller.refresh(); assert.equal(controller.refresh(), refresh);
  assert.equal(reads, 2);
  assert.deepEqual(timers.delays(), [10_000]);
  response.resolve([first, { ...second, status: "completed", pending: 0 }]); await refresh;
  assert.deepEqual(controller.getState().jobs.map((item) => item.pending), [800, 0]);
  assert.equal(controller.getState().blocked, false);
  assert.deepEqual(timers.delays(), [2_000]);
  controller.stop();
});

test("quota waits keep pending separate from failures while another task progresses", async () => {
  let stage = 0;
  const waiting = job("running", { requested: 1000, processed: 200, deleted: 200, pending: 800,
    accounts: [{ accountId: 1, status: "waiting", used: 200, limit: 200, pending: 800, waitReason: "quota" }] });
  const { controller, storage } = harness({ getJobs: async () => [waiting,
    job("running", { jobId: "second", processed: stage, deleted: stage, pending: 7 - stage })] });
  await controller.start(); stage = 3; await controller.refresh();
  assert.deepEqual(controller.getState().jobs.map((item) => [item.deleted, item.failed, item.pending]), [[200, 0, 800], [3, 0, 4]]);
  assert.equal(controller.getState().jobs[0].accounts[0].used, 200);
  assert.equal(controller.getState().blocked, false); assert.equal(storage.read(), null);
  controller.stop();
});

test("failed progress queries preserve all tasks and do not block a new submit", async () => {
  let reads = 0;
  const known = job();
  const { controller, timers } = harness({ getJobs: async () => {
    if (reads++ === 0) return [known]; throw apiError(0, "NETWORK_ERROR");
  } });
  await controller.start(); await controller.refresh();
  assert.deepEqual(controller.getState().jobs, [known]);
  assert.equal(controller.getState().uncertain, true); assert.equal(controller.getState().blocked, false);
  assert.equal(await controller.submit([100], "csrf"), true);
  assert.deepEqual(timers.delays(), [2_000]);
  controller.stop();
});

test("submitting during an older list read cannot lose the new task or regress its snapshot", async () => {
  const response = deferred();
  const completed = job("completed", { processed: 7, deleted: 7, pending: 0 });
  const { controller } = harness({ getJobs: () => response.promise, startJob: async () => completed });
  const started = controller.start(); await flushPromises();
  assert.equal(controller.getState().blocked, false);
  await controller.submit([100], "csrf");
  response.resolve([job("queued")]); await started;
  assert.equal(controller.getState().jobs[0], completed);
  assert.equal(controller.getState().blocked, false);
  controller.stop();
});

test("lost submit responses query the exact operation while keeping older task progress", async () => {
  let listed = job("running", { jobId: "older", processed: 1 });
  const response = deferred();
  const { controller, storage, calls } = harness({
    getJobs: async () => [listed], startJob: async () => { throw apiError(504, "GATEWAY_TIMEOUT"); },
    getJob: (id) => { calls.push(["GET", id]); return response.promise; },
  });
  await controller.start();
  listed = { ...listed, processed: 2 };
  await controller.submit([100], "csrf"); await flushPromises();
  assert.equal(controller.getState().operationId, operationId);
  assert.equal(controller.getState().blocked, true);
  assert.deepEqual(storage.read(), { operationId });
  assert.equal(await controller.submit([101], "csrf"), false);
  response.resolve(job()); await controller.refresh();
  assert.equal(controller.getState().jobs.find((item) => item.jobId === "older").processed, 2);
  assert.equal(controller.getState().jobs.length, 2);
  assert.equal(controller.getState().blocked, false);
  assert.equal(storage.read(), null);
  assert.deepEqual(calls, [["GET", operationId]]);
  controller.stop();
});

test("a missing pending ID stays unresolved even if the list contains unrelated tasks", async () => {
  const memory = memoryStorage();
  const storage = createAliasDeletionStorage("/admin", "owner", memory);
  storage.write({ operationId, jobId: "wrong-job" });
  let reads = 0;
  const { controller, timers } = harness({ storage,
    getJobs: async () => [job("running", { jobId: "another" })],
    getJob: async (id) => { assert.equal(id, operationId); reads++; throw apiError(404, "NOT_FOUND"); },
  });
  assert.equal(controller.getState().blocked, true);
  await controller.start();
  assert.equal(controller.getState().unmatched, true); assert.equal(controller.getState().jobs.length, 1);
  timers.fireNext(); await flushPromises();
  assert.equal(reads, 2); assert.deepEqual(storage.read(), { operationId });
  assert.equal(controller.acknowledgeUnmatched(), true); await flushPromises();
  assert.equal(storage.read(), null); assert.equal(controller.getState().blocked, false);
  assert.equal(controller.getState().jobs[0].jobId, "another");
  controller.stop();
});

test("stored legacy operation IDs recover by ID and list without replaying DELETE", async () => {
  const memory = memoryStorage(), storage = createAliasDeletionStorage("/admin", "owner", memory);
  storage.write({ operationId });
  const { controller, calls } = harness({ storage, getJobs: async () => [job("running", { jobId: "second" })] });
  await controller.start();
  assert.deepEqual(calls.map(([method, id]) => [method, id]), [["GET", operationId]]);
  assert.equal(controller.getState().jobs.length, 2);
  assert.equal(storage.read(), null); assert.equal(controller.getState().blocked, false);
  controller.stop();
});

test("definite admission rejections release pending evidence without replay", async () => {
  for (const [status, code] of [[429, "BATCH_DELETE_BUSY"], [503, "BATCH_DELETE_UNAVAILABLE"], [400, "VALIDATION_FAILED"], [409, "IDEMPOTENCY_CONFLICT"]]) {
    const error = apiError(status, code);
    const { controller, storage, timers } = harness({ startJob: async () => { throw error; } });
    await controller.start(); await assert.rejects(controller.submit([91], "csrf"), (actual) => actual === error);
    assert.equal(storage.read(), null); assert.equal(controller.getState().blocked, false);
    assert.deepEqual(timers.delays(), []); controller.stop();
  }
});

test("unknown submit outcomes retry reads only and keep the submission slot reserved", async () => {
  for (const [status, code] of [[0, "NETWORK_ERROR"], [408, "REQUEST_TIMEOUT"], [500, "INTERNAL_ERROR"], [503, "SERVICE_UNAVAILABLE"], [202, "INVALID_RESPONSE"]]) {
    let deletes = 0, reads = 0;
    const { controller, storage, timers } = harness({
      startJob: async () => { deletes++; throw apiError(status, code); },
      getJob: async () => { reads++; throw apiError(0, "NETWORK_ERROR"); },
    });
    await controller.start(); await controller.submit([91], "csrf"); await flushPromises();
    assert.equal(controller.getState().uncertain, true); assert.equal(controller.getState().blocked, true);
    assert.deepEqual(storage.read(), { operationId }); timers.fireNext(); await flushPromises();
    assert.equal(reads, 2); assert.equal(deletes, 1); controller.stop();
  }
});

test("invalid list responses preserve confirmed snapshots and retry reads", async () => {
  for (const invalid of [null, {}, [null], [job("unexpected")], [job("completed", { processed: null })]]) {
    let reads = 0;
    const known = job();
    const { controller } = harness({ getJobs: async () => reads++ === 0 ? [known] : invalid });
    await controller.start(); await controller.refresh();
    assert.deepEqual(controller.getState().jobs, [known]);
    assert.equal(controller.getState().error.code, "INVALID_RESPONSE");
    assert.equal(controller.getState().blocked, false); controller.stop();
  }
});

test("mismatched submit response is reconciled only by the original ID", async () => {
  const { controller, calls, storage } = harness({ startJob: async () => job("completed", { jobId: "wrong-job" }) });
  await controller.start(); await controller.submit([91], "csrf"); await flushPromises();
  assert.equal(calls.find(([method]) => method === "GET")[1], operationId);
  assert.deepEqual(controller.getState().jobs.map((item) => item.jobId), [operationId]);
  assert.equal(storage.read(), null); controller.stop();
});

test("cancelling one job preserves another task and keeps cancelled counts out of failures", async () => {
  const { controller, calls } = harness();
  await controller.start(); await controller.submit([91], "csrf"); await controller.submit([92], "csrf");
  assert.equal(await controller.cancel(operationId, "csrf"), true); await flushPromises();
  const cancelled = controller.getState().jobs.find((item) => item.jobId === operationId);
  assert.deepEqual([cancelled.cancelled, cancelled.failed, cancelled.pending], [7, 0, 0]);
  assert.equal(controller.getState().jobs.filter(isAliasDeletionJobActive).length, 1);
  assert.equal(controller.getState().blocked, false);
  assert.equal(await controller.cancel(operationId, "csrf"), false);
  assert.equal(calls.filter(([method]) => method === "cancel").length, 1);
  controller.stop();
});

test("cancel requests deduplicate per job without blocking another submission", async () => {
  const response = deferred();
  const { controller } = harness({ cancelJob: () => response.promise });
  await controller.start(); await controller.submit([91], "csrf");
  const cancelled = controller.cancel(operationId, "csrf");
  assert.equal(await controller.cancel(operationId, "csrf"), false);
  assert.deepEqual(controller.getState().cancelling, [operationId]);
  assert.equal(await controller.submit([92], "csrf"), true);
  response.resolve(job("running", { cancelRequested: true })); await cancelled; await flushPromises();
  assert.deepEqual(controller.getState().cancelling, []); controller.stop();
});

test("all completed or interrupted snapshots stop polling without replaying remaining items", async () => {
  const snapshots = [job("completed"), job("interrupted", { jobId: "interrupted" })];
  const { controller, calls, timers } = harness({ getJobs: async () => snapshots });
  await controller.start();
  assert.ok(controller.getState().jobs.every(isAliasDeletionJobTerminal));
  assert.equal(controller.getState().blocked, false); assert.deepEqual(calls, []);
  assert.deepEqual(timers.delays(), []); controller.stop();
});

test("read deadlines abort the fetch and schedule the next query", async () => {
  let signal;
  const { controller, timers } = harness({ getJobs: (options) => {
    signal = options.signal;
    return new Promise((_resolve, reject) => signal.addEventListener("abort", () => reject(new DOMException("timeout", "AbortError")), { once: true }));
  } });
  const request = controller.start(); await flushPromises();
  assert.equal(timers.fireNext(), 10_000); assert.equal(signal.aborted, true);
  await request; assert.deepEqual(timers.delays(), [2_000]);
  assert.equal(controller.getState().blocked, false); controller.stop();
});

test("stop aborts reads and ignores late results from a previous administrator", async () => {
  const response = deferred(); let signal;
  const { controller, changes, timers } = harness({ getJobs: (options) => { signal = options.signal; return response.promise; } });
  const request = controller.start(); await flushPromises(); controller.stop();
  assert.equal(signal.aborted, true); const updates = changes.length;
  response.resolve([job()]); await request;
  assert.equal(changes.length, updates); assert.deepEqual(controller.getState().jobs, []);
  assert.deepEqual(timers.delays(), []); assert.equal(await controller.refresh(), false);
});

test("stop before request microtask avoids the initial fetch", async () => {
  const { controller, calls, timers } = harness();
  const request = controller.start(); controller.stop(); await request;
  assert.deepEqual(calls, []); assert.deepEqual(timers.delays(), []);
});

test("unmount during submit preserves operation evidence without cancelling remote work", async () => {
  const response = deferred(); let signal;
  const { controller, storage, timers, changes } = harness({ startJob: (_ids, _id, _csrf, options) => { signal = options.signal; return response.promise; } });
  await controller.start(); const request = controller.submit([91], "csrf"); controller.stop();
  assert.equal(signal.aborted, false); const updates = changes.length;
  response.resolve(job("completed")); await request;
  assert.equal(changes.length, updates); assert.deepEqual(storage.read(), { operationId });
  assert.deepEqual(timers.delays(), []);
});
