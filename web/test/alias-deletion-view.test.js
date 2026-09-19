import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { createSSRApp, h, ref } from "vue";
import { renderToString } from "vue/server-renderer";

import { normalizeAliasDeletionJob } from "../src/api/admin.js";
import * as deletionUtils from "../src/utils/aliasDeletionJob.js";
const { isAliasDeletionJobTerminal } = deletionUtils;
import { formatTime } from "../src/utils/format.js";

const viewPath = new URL("../src/views/AliasesView.vue", import.meta.url);
const source = await readFile(viewPath, "utf8");
const deleteFunctions = source.slice(
  source.indexOf("function batchDeleteAccountState("),
  source.indexOf("async function copyAliases("),
);
const componentSource = await readFile(new URL("../src/components/AliasDeletionQueue.vue", import.meta.url), "utf8");
const progressTemplate = componentSource.slice(componentSource.indexOf("<template>") + 10, componentSource.indexOf("<script setup>")).trim().replace(/<\/template>$/, "");
const progressFunctions = componentSource.slice(componentSource.indexOf("const expanded = ref("), componentSource.indexOf("</script>"));
async function renderProgress(rawJobs, statePatch = {}, expanded = true) {
  const jobs = (Array.isArray(rawJobs) ? rawJobs : rawJobs ? [rawJobs] : []).map(normalizeAliasDeletionJob);
  const state = { jobs, ...statePatch };
  const bindings = Function("ref", progressFunctions + "; return { expanded, setExpanded, recentFailures };")(ref);
  if (expanded) for (const job of jobs) bindings.setExpanded(job.jobId, true);
  const app = createSSRApp({
    template: progressTemplate,
    setup: () => ({ ...deletionUtils, ...bindings, state, busy: false, cancellingIds: [], formatTime }),
  });
  for (const name of ["el-tag", "el-button", "el-progress"]) {
    app.component(name, { setup: (_props, { slots }) => () => h("span", slots.default?.()) });
  }
  app.config.warnHandler = (message) => assert.fail(message);
  const html = await renderToString(app);
  return { html, text: html.replace(/<[^>]+>/g, " ").replace(/\s+/g, " ") };
}

function harness(overrides = {}) {
  const events = [];
  const warnings = [];
  const errors = [];
  const dependencies = {
    viewActive: true,
    deletingAliases: { value: false },
    deletionJobBlocked: { value: false },
    movingAliases: { value: false },
    exportingAll: { value: false },
    rotatingAllCredentials: { value: false },
    selectedAliasIds: { value: [91, 92] },
    aliases: { value: [{ id: 91, accountId: 1 }, { id: 92, accountId: 2 }] },
    auth: { state: { username: "owner", csrfToken: "csrf" } },
    deletionController: {
      submit: async (ids, csrf) => {
        events.push("submit");
        assert.deepEqual(ids, [91, 92]);
        assert.equal(csrf, "csrf");
        return true;
      },
    },
    getAccount: async (id, options) => {
      events.push(`account:${id}`);
      assert.deepEqual(options, { limit: 1, offset: 0 });
      return { account: { mailboxType: "icloud", lastSyncStatus: "success" }, appleSession: { status: "authenticated" } };
    },
    ElMessageBox: {
      confirm: async (_message, _title, options) => {
        events.push("confirm");
        assert.equal(options.confirmButtonClass, "el-button--danger");
      },
      prompt: async (_message, _title, options) => {
        events.push("prompt");
        assert.equal(options.inputValidator("DELETE_APPLE_ALIASES"), true);
        assert.notEqual(options.inputValidator("delete_apple_aliases"), true);
        assert.equal(options.confirmButtonClass, "el-button--danger");
      },
    },
    ElMessage: { warning: (message) => warnings.push(message) },
    clearAliasSelection: () => events.push("clear"),
    isAliasConfirmationPending: (alias) => alias.pending === true,
    liveRefresh: { stop: () => events.push("stop"), start: () => events.push("restart") },
    beginAliasMutation: () => events.push("begin"),
    confirmationCancelled: (error) => error === "cancel" || error === "close",
    showRequestError: (error, fallback) => errors.push({ error, fallback }),
    successMessage: () => assert.fail("submission is not evidence of successful deletion"),
    ...overrides,
  };
  const extracted = Function(...Object.keys(dependencies), `
    "use strict";
    ${deleteFunctions}
    return {
      run: deleteSelectedAliases,
      setActive(value) { viewActive = value; },
      replaceController(value) { deletionController = value; },
    };
  `)(...Object.values(dependencies));
  return { ...extracted, dependencies, events, warnings, errors };
}

test("eligibility checks and both dangerous confirmations precede exactly one async submit", async () => {
  const { run, events, dependencies, errors } = harness();
  await Promise.all([run(), run()]);
  assert.deepEqual(events, ["stop", "begin", "account:1", "account:2", "confirm", "prompt", "submit", "clear", "restart"]);
  assert.equal(dependencies.deletingAliases.value, false);
  assert.deepEqual(errors, []);
  assert.doesNotMatch(deleteFunctions, /deleteAliases\(|Math\.max|selected\.length -|result\?\.(deleted|failed)|本地记录已保留/);
});

test("each distinct account is checked once and a blocked recovery never opens confirmation", async () => {
  const sameAccount = harness({ aliases: { value: [{ id: 91, accountId: 1 }, { id: 92, accountId: 1 }] } });
  await sameAccount.run();
  assert.deepEqual(sameAccount.events.filter((event) => event.startsWith("account:")), ["account:1"]);
  for (const key of ["deletingAliases", "deletionJobBlocked", "movingAliases", "exportingAll", "rotatingAllCredentials"]) {
    const blocked = harness({ [key]: { value: true } });
    await blocked.run();
    assert.deepEqual(blocked.events, []);
  }
});

test("stale selection, Apple-directory pending, custom, errored and logged-out accounts stay blocked", async () => {
  const stale = harness({ selectedAliasIds: { value: [91, 99] } });
  await stale.run();
  assert.deepEqual(stale.events, ["clear"]);
  assert.equal(stale.errors.length, 1);
  const pending = harness({ aliases: { value: [{ id: 91, accountId: 1, pending: true }, { id: 92, accountId: 2 }] } });
  await pending.run();
  assert.deepEqual(pending.events, []);
  assert.equal(pending.warnings.length, 1);
  for (const detail of [
    { account: { mailboxType: "custom" } },
    { account: { mailboxType: "icloud", lastSyncStatus: "error" } },
    { account: { mailboxType: "icloud", lastSyncError: "sync error" } },
    { account: { mailboxType: "icloud" }, appleSession: { status: "expired" } },
  ]) {
    const blocked = harness({ getAccount: async () => detail });
    await blocked.run();
    assert.deepEqual(blocked.events, ["stop", "begin", "restart"]);
    assert.equal(blocked.warnings.length, 1);
  }
});

test("cancelling either dangerous confirmation never submits", async () => {
  for (const stage of ["confirm", "prompt"]) {
    const cancelled = harness({ ElMessageBox: {
      confirm: async () => { if (stage === "confirm") throw "cancel"; },
      prompt: async () => { throw "close"; },
    } });
    await cancelled.run();
    assert.equal(cancelled.events.includes("submit"), false);
    assert.deepEqual(cancelled.errors, []);
    assert.equal(cancelled.events.at(-1), "restart");
    assert.equal(cancelled.dependencies.deletingAliases.value, false);
  }
});

test("unmount or administrator changes during preflight/confirmations prevent a stale submit", async () => {
  for (const stage of ["preflight", "confirm", "prompt"]) {
    for (const change of ["unmount", "username", "controller"]) {
      let view;
      const invalidate = () => {
        if (change === "unmount") view.setActive(false);
        if (change === "username") view.dependencies.auth.state.username = "another";
        if (change === "controller") view.replaceController({ submit: () => assert.fail("new administrator received stale selection") });
      };
      view = harness(stage === "preflight" ? {
        getAccount: async () => {
          await Promise.resolve();
          invalidate();
          return { account: { mailboxType: "icloud" }, appleSession: { status: "authenticated" } };
        },
      } : {});
      if (stage !== "preflight") {
        view.dependencies.ElMessageBox[stage] = async () => invalidate();
      }
      await view.run();
      assert.equal(view.events.includes("submit"), false, `${stage}/${change} submitted`);
      assert.deepEqual(view.errors, []);
      if (change === "unmount") assert.equal(view.events.includes("restart"), false);
    }
  }
});

test("rejected submissions show the request error without claiming mailbox outcomes", async () => {
  const error = { status: 429, code: "BATCH_DELETE_BUSY", message: "后台任务已满" };
  const view = harness({ deletionController: { submit: async () => { throw error; } } });
  await view.run();
  assert.equal(view.errors[0].error, error);
  assert.equal(view.events.includes("clear"), false);
  assert.doesNotMatch(view.errors[0].fallback, /本地记录已保留|删除失败|已删除/);
  const raced = harness({ deletionController: { submit: async () => false } });
  await raced.run();
  assert.equal(raced.events.includes("clear"), false);
  assert.equal(raced.warnings.length, 1);
});

test("terminal transitions refresh lists without clearing a selection for the next task", () => {
  const factory = source.slice(source.indexOf("function makeDeletionController("), source.indexOf("watch(() => auth.state.username"));
  const refreshes = [], state = { value: { jobs: [] } }, auth = { state: { username: "owner" } };
  let options;
  const dependencies = {
    auth, viewActive: true, deletionState: state,
    startAliasDeletionJob() {}, getAliasDeletionJob() {}, getAliasDeletionJobs() {}, cancelAliasDeletionJob() {},
    createAliasDeletionStorage() {}, ADMIN_BASE_PATH: "/admin", isAliasDeletionJobTerminal,
    createAliasDeletionController: (value) => { options = value; return {}; },
    clearAliasSelection: () => assert.fail("existing task must not clear a new selection"),
    loadAliases: async () => refreshes.push("aliases"), loadAccounts: async () => refreshes.push("accounts"),
    loadGroups: async () => refreshes.push("groups"),
  };
  Function(...Object.keys(dependencies), `${factory}; makeDeletionController();`)(...Object.values(dependencies));
  options.onChange({ jobs: [{ jobId: "one", status: "queued" }, { jobId: "two", status: "running" }] });
  options.onChange({ jobs: [{ jobId: "one", status: "running" }, { jobId: "two", status: "running" }] });
  assert.deepEqual(refreshes, []);
  const finished = { jobs: [{ jobId: "one", status: "completed" }, { jobId: "two", status: "running" }] };
  options.onChange(finished); options.onChange(finished);
  assert.deepEqual(refreshes, ["aliases", "accounts", "groups"]);
  const previousState = state.value; auth.state.username = "another";
  options.onChange({ jobs: [{ jobId: "old-owner-job", status: "completed" }] });
  assert.equal(state.value, previousState);
});

const rawJob = (patch = {}) => ({
  job_id: "waiting-job", status: "running", requested: 1000, processed: 200,
  deleted: 200, failed: 0, pending: 800, cancelled: 0, accounts: [], results: [],
  created_at: "2026-09-19T01:00:00Z", ...patch,
});
const rawAccount = (patch = {}) => ({
  account_id: 1, account_email: "first@icloud.com", status: "waiting",
  requested: 1000, deleted: 200, failed: 0, pending: 800, cancelled: 0, used: 200, limit: 200,
  retry_at: "2026-09-19T02:00:05Z", wait_reason: "quota", ...patch,
});

test("multiple tasks show independent account progress, hourly quota and recovery time", async () => {
  const { html, text } = await renderProgress([
    rawJob({ accounts: [rawAccount()] }),
    rawJob({ job_id: "other", requested: 400, processed: 12, deleted: 12, pending: 388,
      accounts: [rawAccount({ account_id: 2, account_email: "second@icloud.com", status: "running", requested: 400,
        deleted: 12, pending: 388, used: 12, retry_at: null, wait_reason: "" })] }),
  ]);
  assert.match(text, /2 个进行中任务/);
  assert.match(text, /已处理 200 \/ 1000；成功删除 200； 待执行 800；失败 0/);
  assert.match(text, /已处理 12 \/ 400；成功删除 12； 待执行 388；失败 0/);
  assert.match(text, /first@icloud.com/); assert.match(text, /second@icloud.com/);
  assert.match(text, /最近 60 分钟额度 200 \/ 200/);
  assert.match(text, /等待每小时额度恢复/);
  assert.ok(text.includes(formatTime("2026-09-19T02:00:05Z", { seconds: true })));
  assert.match(text, /最近 60 分钟额度 12 \/ 200/);
  assert.match(text, /不同主号同时执行/); assert.match(text, /排队期间同步和创建照常进行/);
  assert.equal((html.match(/取消剩余任务/g) || []).length, 2);
  assert.doesNotMatch(text, /最多 3 次|第 1 次重试|每 2 秒/);
});

test("a logged-out account pauses independently and cancellation is separate from failure", async () => {
  const { text } = await renderProgress([
    rawJob({ accounts: [rawAccount({ status: "paused", wait_reason: "login_required", retry_at: null })] }),
    rawJob({ job_id: "cancelled", status: "completed", processed: 1000, pending: 0, cancelled: 800, cancel_requested: true,
      accounts: [rawAccount({ account_id: 2, status: "cancelled", pending: 0, cancelled: 800, wait_reason: "", retry_at: null })] }),
  ]);
  assert.match(text, /等待主号登录/); assert.match(text, /等待主号重新登录 Apple/);
  assert.match(text, /已结束（含取消项）/); assert.match(text, /失败 0 ；已取消 800/);
  assert.doesNotMatch(text, /失败 800|待执行 800；失败 800/);
  assert.equal(deletionUtils.aliasDeletionPercentage(normalizeAliasDeletionJob(rawJob({ processed: 1000, cancelled: 800 }))), 100);
  assert.equal(deletionUtils.aliasDeletionPercentage(normalizeAliasDeletionJob(rawJob({ processed: 500, cancelled: 300 }))), 50);
  assert.equal(deletionUtils.aliasDeletionPending({ requested: 1000, processed: 500, cancelled: 300 }), 500);
  assert.equal(deletionUtils.aliasDeletionJobLabel({ status: "running", accounts: [
    { status: "waiting" }, { status: "paused" }, { status: "running" },
  ] }), "执行中");
});

test("pending submissions and ordinary progress failures provide different messages", async () => {
  const pending = await renderProgress([], { uncertain: true, operationId: "pending-operation", unmatched: true });
  assert.match(pending.text, /尚未查到本次提交对应的任务/);
  assert.match(pending.text, /确认后可继续添加任务/);
  assert.match(pending.text, /已添加的队列照常执行/);
  assert.match(pending.text, /pending-operation/);
  const failedPoll = await renderProgress(rawJob(), { uncertain: true, operationId: "" });
  assert.match(failedPoll.text, /任务进度暂未更新/);
  assert.doesNotMatch(failedPoll.text, /确认后可继续添加任务/);
});

test("queue details escape result content and lazily render long result lists", async () => {
  const results = Array.from({ length: 416 }, (_, index) => ({
    id: index + 1, address: `alias-${index + 1}@example.invalid`, deleted: false,
    code: "APPLE_RATE_LIMITED", message: "Apple 限流；本地记录已保留", local_retained: true,
  }));
  const raw = rawJob({ status: "completed", requested: 416, processed: 416, deleted: 0, failed: 416, pending: 0, results });
  const closed = await renderProgress(raw, {}, false);
  assert.equal((closed.html.match(/本地记录已保留/g) || []).length, 5);
  const open = await renderProgress(raw);
  assert.equal((open.html.match(/本地记录已保留/g) || []).length, 421);
  assert.doesNotMatch(open.text, /本地记录已保留[；; ]+本地记录已保留/);
  const escaped = await renderProgress(rawJob({ failed: 1, results: [{ id: 1, deleted: false,
    message: "<img src=x onerror=alert(1)>；本地记录已保留", local_retained: false }] }));
  assert.doesNotMatch(escaped.html, /<img|<script/); assert.match(escaped.html, /&lt;img/);
});

test("malformed optional metadata never hides task progress or exposes upstream bodies", async () => {
  const { html, text } = await renderProgress(rawJob({ accounts: [null, { raw_body: "RAW_SECRET" }], waits: [
    null, "RAW_SECRET", { account_id: 12, alias_id: 91, operation: "<script>secret</script>", retry_at: "RAW_SECRET" },
  ] }));
  assert.match(text, /已处理 200 \/ 1000/); assert.doesNotMatch(html, /RAW_SECRET|<script/);
});

test("legacy interrupted tasks preserve the explicit reconciliation notice", async () => {
  const { text } = await renderProgress(rawJob({ status: "interrupted", pending: null }));
  assert.match(text, /此旧任务已中断，部分 Apple 结果待确认/);
  assert.match(text, /不会自动重放旧任务的剩余项/);
  assert.doesNotMatch(text, /取消剩余任务/);
});

test("cancellation confirms remaining scope and ignores a stale administrator", async () => {
  const implementation = source.slice(source.indexOf("async function cancelDeletionJob("), source.indexOf("const selectedAliases = computed("));
  for (const changeUser of [false, true]) {
    const auth = { state: { username: "owner", csrfToken: "csrf" } }, calls = [], ids = { value: [] };
    let message;
    const dependencies = {
      auth, viewActive: true, cancellingDeletionIds: ids,
      deletionController: { cancel: async (...args) => calls.push(args) },
      ElMessageBox: { confirm: async (value) => { message = value; if (changeUser) auth.state.username = "other"; } },
      confirmationCancelled: () => false, showRequestError: assert.fail,
    };
    const cancel = Function(...Object.keys(dependencies), implementation + "; return cancelDeletionJob;")(...Object.values(dependencies));
    await cancel("job-one");
    assert.match(message, /取消本任务尚未执行的项目/); assert.match(message, /已删除的邮箱不会恢复/);
    assert.deepEqual(calls, changeUser ? [] : [["job-one", "csrf"]]);
    if (!changeUser) assert.deepEqual(ids.value, []);
  }
});
