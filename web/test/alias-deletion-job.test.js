import assert from "node:assert/strict";
import test from "node:test";

import {
  ALIAS_DELETION_POLL_INTERVAL_MS,
  ALIAS_DELETION_REQUEST_TIMEOUT_MS,
  createAliasDeletionController,
  createAliasDeletionOperationId,
  createAliasDeletionStorage,
  isAliasDeletionJobActive,
  isAliasDeletionJobTerminal,
} from "../src/utils/aliasDeletionJob.js";

const operationId = "63c21a27-6ef4-425f-a11c-edda65c29267";
const job = (status = "running", patch = {}) => ({
  jobId: operationId, status, requested: 7, processed: 0, deleted: 0, failed: 0,
  results: [], requestId: "request-42", ...patch,
});
const apiError = (status, code) => Object.assign(new Error(code), { status, code });
const flushPromises = () => new Promise((resolve) => setImmediate(resolve));

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function memoryStorage() {
  const values = new Map();
  return {
    values,
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
    removeItem: (key) => values.delete(key),
  };
}

function fakeTimers() {
  let nextId = 0;
  const timers = new Map();
  return {
    setTimeoutFn(callback, delay) {
      const id = ++nextId;
      timers.set(id, { callback, delay });
      return id;
    },
    clearTimeoutFn: (id) => timers.delete(id),
    fireNext() {
      const entry = timers.entries().next().value;
      assert.ok(entry, "expected a scheduled timer");
      const [id, timer] = entry;
      timers.delete(id);
      timer.callback();
      return timer.delay;
    },
    delays: () => [...timers.values()].map(({ delay }) => delay),
  };
}

function harness(overrides = {}) {
  const timers = fakeTimers();
  const memory = memoryStorage();
  const storage = createAliasDeletionStorage("/install/admin", "owner", memory);
  const calls = [];
  const changes = [];
  const controller = createAliasDeletionController({
    storage,
    createOperationId: () => operationId,
    startJob: async (...args) => { calls.push(["DELETE", ...args]); return job("queued"); },
    getJob: async (...args) => { calls.push(["GET", ...args]); return job(); },
    getLatestJob: async (...args) => { calls.push(["latest", ...args]); return null; },
    onChange: (state) => changes.push(state),
    ...timers,
    ...overrides,
  });
  return { controller, timers, storage, memory, calls, changes };
}

test("operation IDs are UUIDs including the HTTP-compatible random-values fallback", () => {
  assert.equal(createAliasDeletionOperationId({ randomUUID: () => operationId }), operationId);
  assert.match(createAliasDeletionOperationId({ getRandomValues: (bytes) => bytes.fill(255) }),
    /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
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

test("recovery blocks new submissions and latest null enables the first delete", async () => {
  const { controller, calls, timers } = harness();
  assert.equal(controller.getState().blocked, true);
  assert.equal(await controller.submit([91], "csrf"), false);
  assert.equal(await controller.start(), true);
  assert.equal(await controller.start(), false);
  assert.deepEqual(calls.map(([method]) => method), ["latest"]);
  assert.equal(controller.getState().blocked, false);
  assert.deepEqual(timers.delays(), []);
  controller.stop();
});

test("submitting stores the ID before one short DELETE and blocks duplicates", async () => {
  const response = deferred();
  let submissions = 0;
  const { controller, storage, timers } = harness({
    startJob: (ids, id, csrfToken, { signal }) => {
      submissions += 1;
      assert.deepEqual(storage.read(), { operationId });
      assert.deepEqual(ids, [91, 92]);
      assert.equal(id, operationId);
      assert.equal(csrfToken, "csrf");
      assert.equal(signal.aborted, false);
      return response.promise;
    },
  });
  await controller.start();
  const ids = [91, 92];
  const submitted = controller.submit(ids, "csrf");
  ids.push(93);
  assert.equal(controller.getState().submitting, true);
  assert.equal(await controller.submit([91], "csrf"), false);
  assert.equal(await controller.refresh(), false);
  assert.deepEqual(timers.delays(), [10_000]);
  response.resolve(job("queued"));
  assert.equal(await submitted, true);
  assert.equal(submissions, 1);
  assert.equal(controller.getState().blocked, true);
  assert.deepEqual(timers.delays(), [2_000]);
  controller.stop();
});

test("two-second polling is serial, deduplicates manual refresh, and keeps backend counters", async () => {
  assert.equal(ALIAS_DELETION_POLL_INTERVAL_MS, 2_000);
  assert.equal(ALIAS_DELETION_REQUEST_TIMEOUT_MS, 10_000);
  const response = deferred();
  let reads = 0;
  const { controller, timers } = harness({
    getJob: () => { reads += 1; return response.promise; },
  });
  await controller.start();
  await controller.submit([91], "csrf");
  assert.equal(timers.fireNext(), 2_000);
  await flushPromises();
  assert.equal(reads, 1);
  const first = controller.refresh();
  assert.equal(controller.refresh({ latest: true }), first);
  assert.deepEqual(timers.delays(), [10_000]);
  const snapshot = job("running", { processed: 2, deleted: 0, failed: 1, results: [{ id: 91, deleted: true }] });
  response.resolve(snapshot);
  await first;
  assert.equal(controller.getState().job, snapshot);
  assert.equal(controller.getState().job.deleted, 0);
  assert.deepEqual(timers.delays(), [2_000]);
  controller.stop();
});

test("lost submit responses query operationId even when latest has an unrelated completed job", async () => {
  const calls = [];
  const { controller, storage } = harness({
    getLatestJob: async () => { calls.push("latest"); return job("completed", { jobId: "old-job" }); },
    startJob: async () => { calls.push("DELETE"); throw apiError(0, "NETWORK_ERROR"); },
    getJob: async (id) => { calls.push(id); return job(); },
  });
  await controller.start();
  await controller.submit([91], "csrf");
  await flushPromises();
  assert.deepEqual(calls, ["latest", "DELETE", operationId]);
  assert.equal(controller.getState().job.jobId, operationId);
  await controller.refresh({ latest: true });
  assert.equal(calls.at(-1), operationId);
  assert.deepEqual(storage.read(), { operationId });
  controller.stop();
});

test("refresh recovery trusts saved operationId, not an older saved/latest job ID", async () => {
  const memory = memoryStorage();
  const storage = createAliasDeletionStorage("/admin", "owner", memory);
  storage.write({ operationId });
  const key = [...memory.values.keys()][0];
  memory.setItem(key, JSON.stringify({ operationId, jobId: "wrong-job", previousJobId: "old-job" }));
  const { controller, calls } = harness({ storage });
  await controller.start();
  assert.deepEqual(calls.map(([method, id]) => [method, id]), [["GET", operationId]]);
  assert.equal(controller.getState().job.jobId, operationId);
  controller.stop();
});

test("404 after a lost response stays unknown and retries only the same job", async () => {
  let queries = 0;
  const { controller, timers, storage, calls } = harness({
    startJob: async () => { throw apiError(504, "GATEWAY_TIMEOUT"); },
    getJob: async (id) => { assert.equal(id, operationId); queries += 1; throw apiError(404, "NOT_FOUND"); },
  });
  await controller.start();
  await controller.submit([91], "csrf");
  await flushPromises();
  assert.equal(controller.getState().job, null);
  assert.equal(controller.getState().uncertain, true);
  assert.equal(controller.getState().unmatched, true);
  assert.equal(controller.getState().blocked, true);
  assert.equal(await controller.submit([91], "csrf"), false);
  assert.deepEqual(storage.read(), { operationId });
  timers.fireNext();
  await flushPromises();
  assert.equal(queries, 2);
  assert.equal(calls.length, 1, "latest was queried only before submission");
  assert.equal(controller.acknowledgeUnmatched(), true);
  assert.equal(controller.getState().blocked, true, "reconciliation must check latest before allowing another submit");
  await flushPromises();
  assert.equal(storage.read(), null);
  assert.equal(controller.getState().blocked, false);
  assert.equal(calls.length, 2);
  controller.stop();
});

test("409 follows the administrator's active job instead of retrying the rejected operation", async () => {
  let latestCalls = 0;
  let deletes = 0;
  const { controller, storage, timers } = harness({
    getLatestJob: async () => ++latestCalls === 1 ? null : job("queued", { jobId: "other-active-job" }),
    startJob: async () => { deletes += 1; throw apiError(409, "BATCH_DELETE_IN_PROGRESS"); },
    getJob: async (id) => { assert.equal(id, "other-active-job"); return job("running", { jobId: id }); },
  });
  await controller.start();
  await controller.submit([91], "csrf");
  await flushPromises();
  assert.equal(latestCalls, 2);
  assert.deepEqual(storage.read(), { operationId: "other-active-job" });
  timers.fireNext();
  await flushPromises();
  assert.equal(deletes, 1);
  assert.equal(controller.getState().blocked, true);
  controller.stop();
});

test("definite 429/503 and validation rejections release the pending ID without automatic retries", async () => {
  for (const [status, code] of [[429, "BATCH_DELETE_BUSY"], [503, "BATCH_DELETE_UNAVAILABLE"], [400, "VALIDATION_FAILED"], [409, "IDEMPOTENCY_CONFLICT"]]) {
    const error = apiError(status, code);
    const { controller, storage, timers, calls } = harness({ startJob: async () => { throw error; } });
    await controller.start();
    await assert.rejects(controller.submit([91], "csrf"), (actual) => actual === error);
    assert.equal(storage.read(), null);
    assert.equal(controller.getState().blocked, false);
    assert.equal(controller.getState().uncertain, false);
    assert.deepEqual(timers.delays(), []);
    assert.equal(calls.length, 1);
    controller.stop();
  }
});

test("network, gateway and malformed submission responses retain pending evidence and only retry GET", async () => {
  for (const [status, code] of [[0, "NETWORK_ERROR"], [408, "REQUEST_TIMEOUT"], [500, "INTERNAL_ERROR"], [503, "SERVICE_UNAVAILABLE"], [202, "INVALID_RESPONSE"], [429, "INVALID_RESPONSE"]]) {
    let deletes = 0;
    let reads = 0;
    const { controller, storage, timers } = harness({
      startJob: async () => { deletes += 1; throw apiError(status, code); },
      getJob: async () => { reads += 1; throw apiError(0, "NETWORK_ERROR"); },
    });
    await controller.start();
    await controller.submit([91], "csrf");
    await flushPromises();
    assert.equal(controller.getState().uncertain, true);
    assert.equal(controller.getState().job, null);
    assert.deepEqual(storage.read(), { operationId });
    timers.fireNext();
    await flushPromises();
    assert.equal(reads, 2);
    assert.equal(deletes, 1);
    controller.stop();
  }
});

test("unknown job IDs, statuses and counters never replace the confirmed snapshot", async () => {
  for (const bad of [null, job("completed", { jobId: "wrong-id" }), job("unexpected"), job("completed", { processed: null })]) {
    const { controller, storage } = harness({ getJob: async () => bad });
    await controller.start();
    await controller.submit([91], "csrf");
    const confirmed = controller.getState().job;
    await controller.refresh();
    assert.equal(controller.getState().job, confirmed);
    assert.equal(controller.getState().error.code, "INVALID_RESPONSE");
    assert.equal(controller.getState().uncertain, true);
    assert.deepEqual(storage.read(), { operationId });
    controller.stop();
  }
});

test("a failed latest lookup keeps retrying latest instead of accepting the old completed job", async () => {
  const oldJob = job("completed", { jobId: "old-job" });
  let reads = 0;
  const { controller, timers } = harness({
    getLatestJob: async () => {
      reads += 1;
      if (reads === 1) return oldJob;
      if (reads === 2) throw apiError(0, "NETWORK_ERROR");
      return job();
    },
    getJob: async () => assert.fail("retried the old completed job instead of latest"),
  });
  await controller.start();
  await controller.refresh({ latest: true });
  assert.equal(controller.getState().job, oldJob);
  assert.equal(controller.getState().blocked, true);
  timers.fireNext();
  await flushPromises();
  assert.equal(reads, 3);
  assert.equal(controller.getState().job.jobId, operationId);
  controller.stop();
});

test("an immediate terminal/idempotent submit response is accepted without polling or replay", async () => {
  const snapshot = job("completed", { processed: 7, deleted: 5, failed: 2 });
  const { controller, storage, timers, calls } = harness({ startJob: async () => snapshot });
  await controller.start();
  await controller.submit([91], "csrf");
  assert.equal(controller.getState().job, snapshot);
  assert.equal(controller.getState().blocked, false);
  assert.equal(storage.read(), null);
  assert.deepEqual(timers.delays(), []);
  assert.equal(calls.length, 1);
  controller.stop();
});

test("a mismatched successful submit response remains unknown until operationId is queried", async () => {
  const { controller, calls } = harness({ startJob: async () => job("completed", { jobId: "unrelated-job" }) });
  await controller.start();
  await controller.submit([91], "csrf");
  await flushPromises();
  assert.equal(calls.at(-1)[0], "GET");
  assert.equal(calls.at(-1)[1], operationId);
  assert.equal(controller.getState().job.jobId, operationId);
  controller.stop();
});

test("completed and interrupted jobs stop polling without replaying incomplete items", async () => {
  for (const status of ["completed", "interrupted"]) {
    const snapshot = job(status, { processed: 2, deleted: 1, failed: 6 });
    assert.equal(isAliasDeletionJobActive(snapshot), false);
    assert.equal(isAliasDeletionJobTerminal(snapshot), true);
    const { controller, storage, timers } = harness({ getJob: async () => snapshot });
    await controller.start();
    await controller.submit([91], "csrf");
    await controller.refresh();
    assert.equal(controller.getState().job, snapshot);
    assert.equal(storage.read(), null);
    assert.equal(controller.getState().blocked, false);
    assert.deepEqual(timers.delays(), []);
    controller.stop();
  }
});

test("recovery without storage uses latest and persists active jobs for the next refresh", async () => {
  const { controller, storage } = harness({ getLatestJob: async () => job("queued") });
  await controller.start();
  assert.deepEqual(storage.read(), { operationId });
  controller.stop();
  const restored = harness({ storage });
  await restored.controller.start();
  assert.equal(restored.calls[0][0], "GET");
  restored.controller.stop();
});

test("short request deadlines abort the read or submission and schedule only GET retries", async () => {
  for (const mutation of [false, true]) {
    let signal;
    const hanging = (options) => {
      signal = options.signal;
      return new Promise((_resolve, reject) => signal.addEventListener("abort", () => reject(new DOMException("timeout", "AbortError")), { once: true }));
    };
    const { controller, timers, calls } = harness(mutation
      ? { startJob: (_ids, _id, _csrf, options) => hanging(options) }
      : { getLatestJob: hanging });
    if (mutation) await controller.start();
    const request = mutation ? controller.submit([91], "csrf") : controller.start();
    await flushPromises();
    assert.equal(timers.fireNext(), 10_000);
    assert.equal(signal.aborted, true);
    await request;
    await flushPromises();
    if (mutation) {
      assert.equal(calls.at(-1)[0], "GET");
      assert.equal(controller.getState().job.jobId, operationId);
    } else {
      assert.equal(controller.getState().uncertain, true);
    }
    assert.deepEqual(timers.delays(), [2_000]);
    controller.stop();
  }
});

test("stop aborts in-flight reads and ignores late responses without clearing stored IDs", async () => {
  const response = deferred();
  let signal;
  const { controller, changes, timers, storage } = harness({
    getJob: (_id, options) => { signal = options.signal; return response.promise; },
  });
  await controller.start();
  await controller.submit([91], "csrf");
  const request = controller.refresh();
  await flushPromises();
  controller.stop();
  assert.equal(signal.aborted, true);
  const updates = changes.length;
  response.resolve(job("completed"));
  await request;
  assert.equal(changes.length, updates);
  assert.deepEqual(storage.read(), { operationId });
  assert.deepEqual(timers.delays(), []);
  assert.equal(await controller.refresh(), false);
});

test("stop before a scheduled request starts prevents even the initial GET", async () => {
  const { controller, calls, timers } = harness();
  const request = controller.start();
  controller.stop();
  await request;
  assert.deepEqual(calls, []);
  assert.deepEqual(timers.delays(), []);
});

test("unmount during submit leaves the request and background job alone for later recovery", async () => {
  const response = deferred();
  let signal;
  const { controller, storage, timers, changes } = harness({
    startJob: (_ids, _id, _csrf, options) => { signal = options.signal; return response.promise; },
  });
  await controller.start();
  const request = controller.submit([91], "csrf");
  controller.stop();
  assert.equal(signal.aborted, false);
  const updates = changes.length;
  response.resolve(job("completed"));
  await request;
  assert.equal(changes.length, updates);
  assert.deepEqual(storage.read(), { operationId });
  assert.deepEqual(timers.delays(), []);
});
