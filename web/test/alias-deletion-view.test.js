import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { isAliasDeletionJobTerminal } from "../src/utils/aliasDeletionJob.js";

const viewPath = new URL("../src/views/AliasesView.vue", import.meta.url);
const source = await readFile(viewPath, "utf8");
const deleteFunctions = source.slice(
  source.indexOf("function batchDeleteAccountState("),
  source.indexOf("async function copyAliases("),
);

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

test("page lifecycle starts recovery, stops polling, and isolates controllers on username changes", () => {
  assert.match(source, /onMounted\(\(\) => \{\s*if \(auth\.state\.username\) void deletionController\.start\(\)/);
  assert.match(source, /onBeforeUnmount\(\(\) => \{\s*viewActive = false;\s*deletionController\.stop\(\)/);
  assert.match(source, /watch\(\(\) => auth\.state\.username, \(\) => \{\s*deletionController\.stop\(\);\s*deletionController = makeDeletionController\(\)/);
  assert.match(source, /createAliasDeletionStorage\(ADMIN_BASE_PATH, username\)/);
  assert.match(source, /auth\.state\.username !== username/);
  assert.match(source, /deletionState\.operationId/);
  for (const field of ["processed", "requested", "deleted", "failed"]) {
    assert.match(source, new RegExp(`\\{\\{ deletionJob\\.${field} \\}\\}`));
  }
  assert.match(source, /v-if="failure\.localRetained">；本地记录已保留/);
  assert.match(source, /任务已中断，部分 Apple 结果待确认/);
  assert.match(source, /不会自动重放剩余项/);
  assert.doesNotMatch(source, /删除失败，结果待核对|近期失败原因/);
  assert.match(source, /\.alias-deletion-progress\s*\{[^}]*overflow:\s*auto;[^}]*overflow-wrap:\s*anywhere;/);
  assert.match(source, /\.alias-deletion-progress__header\s*\{[^}]*flex-wrap:\s*wrap;/);
});

test("terminal transitions clear selection and reload lists once, late cross-user callbacks are ignored", () => {
  const factory = source.slice(source.indexOf("function makeDeletionController("), source.indexOf("watch(() => auth.state.username"));
  const refreshes = [];
  const state = { value: {} };
  const auth = { state: { username: "owner" } };
  let options;
  const dependencies = {
    auth, viewActive: true, deletionState: state, deletionResultsExpanded: { value: false },
    startAliasDeletionJob() {}, getAliasDeletionJob() {}, getLatestAliasDeletionJob() {},
    createAliasDeletionStorage() {}, ADMIN_BASE_PATH: "/admin", isAliasDeletionJobTerminal,
    createAliasDeletionController: (value) => { options = value; return {}; },
    clearAliasSelection: () => refreshes.push("clear"),
    loadAliases: async () => refreshes.push("aliases"),
    loadAccounts: async () => refreshes.push("accounts"),
    loadGroups: async () => refreshes.push("groups"),
  };
  Function(...Object.keys(dependencies), `${factory}; makeDeletionController();`)(...Object.values(dependencies));
  options.onChange({ job: { jobId: "job", status: "queued" } });
  options.onChange({ job: { jobId: "job", status: "running" } });
  assert.deepEqual(refreshes, ["clear"]);
  options.onChange({ job: { jobId: "job", status: "interrupted" } });
  options.onChange({ job: { jobId: "job", status: "interrupted" } });
  assert.deepEqual(refreshes, ["clear", "clear", "aliases", "accounts", "groups"]);
  const previousState = state.value;
  auth.state.username = "another";
  options.onChange({ job: { jobId: "old-owner-job", status: "completed" } });
  assert.equal(state.value, previousState);
});
