import assert from "node:assert/strict";
import test from "node:test";
import { cancelAliasDeletionJob, clearCompletedAliasDeletionJobs, getAliasDeletionJobs, normalizeAliasDeletionJob } from "../src/api/admin.js";

const raw = {
  job_id: "job-one", status: "running", requested: 1000, processed: 200,
  deleted: 200, failed: 0, pending: 800, cancelled: 0, cancel_requested: false,
  accounts: [{ account_id: 1, account_email: "owner@icloud.com", status: "waiting", requested: 1000,
    deleted: 200, failed: 0, pending: 800, cancelled: 0, used: 200, limit: 200,
    retry_at: "2026-09-19T02:00:05Z", wait_reason: "quota", raw_body: "SECRET" }],
  waits: [{ account_id: 1, alias_id: 201, operation: "delete", retry_at: "2026-09-19T02:00:05Z",
    reason: "quota", used: 200, limit: 200, raw_body: "SECRET" }],
  results: [],
};

test("task lists return every snapshot and allowlist hourly quota metadata", async (t) => {
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  const abort = new AbortController();
  globalThis.fetch = async (url, options) => {
    assert.equal(url, "/admin/api/v1/aliases/batch/jobs");
    assert.equal(options.method, "GET"); assert.equal(options.signal, abort.signal);
    return new Response(JSON.stringify({ data: { jobs: [raw, { ...raw, job_id: "job-two" }] } }), {
      headers: { "Content-Type": "application/json" },
    });
  };
  const jobs = await getAliasDeletionJobs({ signal: abort.signal });
  assert.deepEqual(jobs.map((job) => job.jobId), ["job-one", "job-two"]);
  assert.deepEqual(jobs[0].accounts[0], {
    accountId: 1, accountEmail: "owner@icloud.com", status: "waiting", requested: 1000,
    deleted: 200, failed: 0, pending: 800, cancelled: 0, used: 200, limit: 200,
    retryAt: "2026-09-19T02:00:05Z", waitReason: "quota",
  });
  assert.deepEqual(jobs[0].waits[0], { accountId: 1, aliasId: 201, operation: "delete",
    retryAt: "2026-09-19T02:00:05Z", reason: "quota", used: 200, limit: 200 });
  assert.doesNotMatch(JSON.stringify(jobs), /SECRET|raw_body|attempt|maxAttempts/);
});

test("list response validation rejects missing lists instead of clearing known tasks", async (t) => {
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  for (const value of [null, {}, { jobs: null }, { jobs: "unexpected" }]) {
    globalThis.fetch = async () => new Response(JSON.stringify({ data: value }), { headers: { "Content-Type": "application/json" } });
    await assert.rejects(getAliasDeletionJobs(), { code: "INVALID_RESPONSE" });
  }
});

test("cancel sends CSRF and an encoded task ID without replaying deletion", async (t) => {
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  const abort = new AbortController(); let calls = 0;
  globalThis.fetch = async (url, options) => {
    calls++;
    assert.equal(url, "/admin/api/v1/aliases/batch/jobs/job%2Fone%3F/cancel");
    assert.equal(options.method, "POST"); assert.equal(options.headers.get("X-CSRF-Token"), "csrf");
    assert.equal(options.signal, abort.signal); assert.equal(options.body, undefined);
    return new Response(JSON.stringify({ data: { ...raw, status: "completed", pending: 0, cancelled: 800, cancel_requested: true } }), {
      headers: { "Content-Type": "application/json" },
    });
  };
  const job = await cancelAliasDeletionJob("job/one?", "csrf", { signal: abort.signal });
  assert.deepEqual([job.deleted, job.pending, job.cancelled, job.failed, job.cancelRequested], [200, 0, 800, 0, true]);
  assert.equal(calls, 1);
});

test("login waits tolerate omitted retry times and malformed optional data stays isolated", () => {
  const job = normalizeAliasDeletionJob({ ...raw, accounts: [null, {}, ...raw.accounts], waits: [{
    account_id: 1, alias_id: 201, operation: "validate", reason: "login_required",
  }] });
  assert.equal(job.accounts.length, 1);
  assert.deepEqual(job.waits, [{ accountId: 1, aliasId: 201, operation: "validate", reason: "login_required", retryAt: null }]);
  assert.equal(job.pending, 800);
  const malformed = normalizeAliasDeletionJob({ ...raw, accounts: [{ ...raw.accounts[0], used: "200" }], cancel_requested: "true", cancelled: "800" });
  assert.deepEqual(malformed.accounts, []); assert.equal(malformed.cancelRequested, false); assert.equal(malformed.cancelled, 0);
});

test("clearing completed history sends CSRF and returns the confirmed IDs", async (t) => {
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  const abort = new AbortController();
  globalThis.fetch = async (url, options) => {
    assert.equal(url, "/admin/api/v1/aliases/batch/jobs/clear-completed");
    assert.equal(options.method, "POST");
    assert.equal(options.headers.get("X-CSRF-Token"), "csrf");
    assert.equal(options.signal, abort.signal);
    assert.equal(options.body, undefined);
    return new Response(JSON.stringify({ data: { cleared: 1, cleared_job_ids: ["earlier-job", "job-one"] } }), {
      headers: { "Content-Type": "application/json" },
    });
  };
  assert.deepEqual(await clearCompletedAliasDeletionJobs("csrf", { signal: abort.signal }), {
    cleared: 1, clearedJobIds: ["earlier-job", "job-one"],
  });
});

test("invalid clear responses do not confirm history removal", async (t) => {
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  for (const value of [null, {}, { cleared: -1, cleared_job_ids: [] },
    { cleared: 1, cleared_job_ids: [] }, { cleared: 0, cleared_job_ids: [null] },
    { cleared: 0, cleared_job_ids: [""] }, { cleared: "1", cleared_job_ids: ["job-one"] }]) {
    globalThis.fetch = async () => new Response(JSON.stringify({ data: value }), {
      headers: { "Content-Type": "application/json" },
    });
    await assert.rejects(clearCompletedAliasDeletionJobs("csrf"), { code: "INVALID_RESPONSE" });
  }
});
