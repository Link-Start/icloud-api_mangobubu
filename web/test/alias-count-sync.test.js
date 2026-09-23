import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { appleAliasDirectoryStatus } from "../src/utils/appleAliasState.js";

const viewPath = new URL("../src/views/AccountDetailView.vue", import.meta.url);

function functionBody(source, signature) {
  const start = source.indexOf(signature);
  assert.notEqual(start, -1, `missing ${signature}`);
  const opening = source.indexOf("{", start);
  assert.notEqual(opening, -1, `missing body for ${signature}`);
  let depth = 0;
  for (let index = opening; index < source.length; index += 1) {
    if (source[index] === "{") depth += 1;
    if (source[index] === "}") {
      depth -= 1;
      if (depth === 0) return source.slice(opening, index + 1);
    }
  }
  assert.fail(`unterminated body for ${signature}`);
}

function autoCreationPanel(source) {
  const start = source.indexOf('class="auto-creation-panel"');
  assert.notEqual(start, -1, "missing automatic creation panel");
  const end = source.indexOf(
    'class="data-panel desktop-data-table account-alias-table"',
    start,
  );
  assert.ok(end > start, "missing alias table after automatic creation panel");
  return source.slice(start, end);
}

test("automatic creation panel displays the current alias count", async () => {
  const source = await readFile(viewPath, "utf8");
  assert.match(
    autoCreationPanel(source),
    /\{\{[^}]*\b(?:aliasCount|aliases\.length)\b[^}]*\}\}/,
  );
});

test("directory synchronization refreshes the current server-backed alias page", async () => {
  const source = await readFile(viewPath, "utf8");
  const syncBody = functionBody(source, "async function performAliasesSync");

  assert.match(syncBody, /const result = await syncAccountAliases/);
  assert.match(syncBody, /const detailLoaded = await loadDetail\(\)/);
  assert.match(syncBody, /if \(!detailLoaded\)/);
  assert.match(syncBody, /邮箱列表刷新失败/);
  assert.doesNotMatch(syncBody, /aliases\.value = result\.aliases/);
  assert.match(syncBody, /可通过列表中的复制操作导出完整凭证/);
  assert.doesNotMatch(syncBody, /pending|acknowledge|oneTime|batchSecrets/i);
});

test("account alias count is never derived from the visible page length", async () => {
  const source = await readFile(viewPath, "utf8");

  assert.doesNotMatch(source, /function syncAccountAliasCount/);
  assert.doesNotMatch(source, /aliasCount:\s*(?:aliases|nextAliases)\.length/);
  assert.doesNotMatch(source, /account\.value\.aliasCount\s*=/);
  assert.match(source, /detail\?\.pagination\?\.total/);
  assert.match(source, /\{\{ account\.aliasCount \}\}/);
});

test("alias creation, random generation, and deletion refresh the current page", async () => {
  const source = await readFile(viewPath, "utf8");
  const addAlias = functionBody(source, "async function addAlias");
  const generateRandomAliases = functionBody(
    source,
    "async function generateRandomAliases",
  );
  const removeAlias = functionBody(source, "async function removeAlias");

  assert.match(addAlias, /await loadDetail\(\)/);
  assert.doesNotMatch(addAlias, /aliases\.value\s*=/);
  assert.match(addAlias, /整套凭证已签发，可通过列表中的复制操作导出/);
  assert.match(generateRandomAliases, /await loadDetail\(\)/);
  assert.doesNotMatch(generateRandomAliases, /mergedByID|aliases\.value\s*=/);
  assert.match(removeAlias, /await loadDetail\(\)/);
  assert.doesNotMatch(removeAlias, /aliases\.value\s*=\s*aliases\.value\.filter/);
});

test("manual registration preserves iCloud input until Apple login succeeds while custom registration stays local", async (t) => {
  const source = await readFile(viewPath, "utf8");
  const body = functionBody(source, "async function addAlias");
  for (const scenario of [
    { name: "iCloud login required", custom: false, authenticated: false, requests: 0 },
    { name: "iCloud registered after verification", custom: false, authenticated: true, requests: 1 },
    { name: "custom mailbox needs no Apple session", custom: true, authenticated: false, requests: 1 },
    { name: "expired Apple session preserves input", custom: false, authenticated: true, requests: 1, errorCode: "APPLE_SESSION_EXPIRED" },
    { name: "absent Apple address preserves input", custom: false, authenticated: true, requests: 1, errorCode: "APPLE_ALIAS_NOT_FOUND" },
  ]) {
    await t.test(scenario.name, async () => {
      const events = [];
      const messages = [];
      const form = { address: "existing@example.com", label: "Keep my label" };
      const requestError = Object.assign(new Error("Directory verification failed"), {
        code: scenario.errorCode,
        requestId: "request-1",
      });
      const dependencies = {
        createLock: { acquire: () => true, release: () => events.push("release") },
        beginDetailMutation() {},
        account: { value: { id: 1 } },
        createLoading: { value: false },
        aliasFormError: { value: null },
        aliasFormRef: { value: { validate: async () => true, resetFields() {} } },
        isCurrentAccount: () => true,
        isCustomMailbox: { value: scenario.custom },
        appleSessionAuthenticated: { value: scenario.authenticated },
        appleSession: { value: { status: "authenticated" } },
        aliasForm: form,
        auth: { state: { csrfToken: "csrf" } },
        async createAlias(accountId, payload) {
          assert.equal(accountId, 1);
          assert.deepEqual(payload, form);
          events.push("register");
          if (scenario.errorCode) throw requestError;
        },
        async loadDetail() { events.push("reload"); return true; },
        successMessage: (message) => messages.push(message),
        isSessionInvalid: () => false,
        isAppleSessionInvalid: (error) => error.code === "APPLE_SESSION_EXPIRED",
        redirectExpiredSession() { assert.fail("Apple login must not sign out the administrator"); },
      };
      const register = Function(
        ...Object.keys(dependencies),
        `"use strict"; return async function () ${body}`,
      )(...Object.values(dependencies));

      await register();

      const succeeded = scenario.requests > 0 && !scenario.errorCode;
      assert.equal(events.filter((event) => event === "register").length, scenario.requests);
      assert.equal(events.includes("reload"), succeeded);
      assert.equal(messages.length, succeeded ? 1 : 0);
      assert.equal(dependencies.createLoading.value, false);
      assert.equal(events.at(-1), "release");
      if (succeeded) {
        assert.deepEqual(form, { address: "", label: "" });
        assert.match(messages[0], /已登记|已核对并登记/);
      } else {
        assert.deepEqual(form, { address: "existing@example.com", label: "Keep my label" });
        if (scenario.errorCode === "APPLE_ALIAS_NOT_FOUND") {
          assert.equal(dependencies.aliasFormError.value, requestError);
        } else {
          assert.match(dependencies.aliasFormError.value.message, /同步隐私邮箱.*登录/);
        }
        if (scenario.errorCode === "APPLE_SESSION_EXPIRED") {
          assert.equal(dependencies.appleSession.value.status, "expired");
          assert.equal(dependencies.aliasFormError.value.requestId, "request-1");
        }
      }
    });
  }
});

test("directory synchronization distinguishes Apple active addresses from all local records", async () => {
  const source = await readFile(viewPath, "utf8");
  const body = functionBody(source, "async function performAliasesSync");
  const messages = [];
  const dependencies = {
    aliasesSyncLoading: { value: false },
    autoCreationLoading: { value: false },
    account: { value: { id: 1, aliasCount: 185 } },
    beginDetailMutation() {},
    auth: { state: { csrfToken: "csrf" } },
    async syncAccountAliases() {
      return {
        account: { id: 1, aliasCount: 180 },
        summary: {
          total: 180, inactiveCount: 12, createdCount: 0, importedDisabledCount: 0,
          missingCount: 5, removedCount: 5, inactiveUpdatedCount: 3, restoredCount: 2,
        },
      };
    },
    isCurrentAccount: () => true,
    appleSession: { value: null },
    autoCreation: { value: null },
    aliasSyncSummary: { value: null },
    loadDetail: async () => true,
    successMessage: (message) => messages.push(message),
    isAppleSessionInvalid: () => false,
    showRequestError: (error) => assert.fail(error.message),
  };
  const synchronize = Function(
    ...Object.keys(dependencies),
    `"use strict"; return async function () ${body}`,
  )(...Object.values(dependencies));

  await synchronize();

  assert.equal(messages.length, 1);
  assert.match(messages[0], /转发至 Apple 当前目标的地址：启用 168 个、停用 12 个/);
  assert.match(messages[0], /本地已登记 180 个（含停用及待确认记录）/);
  assert.match(messages[0], /本次自动移除 5 个本地地址（Apple 已不存在）/);
  assert.doesNotMatch(messages[0], /本地归档保留/);
  assert.match(messages[0], /本次更新停用 3 个、Apple 已恢复 2 个（可手动启用）/);
  assert.equal(dependencies.aliasesSyncLoading.value, false);
});

test("legacy missing and inactive addresses reject enabling before directory reconciliation", async () => {
  const source = await readFile(viewPath, "utf8");
  const body = functionBody(source, "async function toggleAlias");
  const dependencies = {
    isAliasConfirmationPending: () => false,
    appleAliasDirectoryStatus,
    aliasActionLock: { acquire() { assert.fail("directory-blocked alias reached the mutation lock"); } },
  };
  const toggle = Function(
    ...Object.keys(dependencies),
    `return async function (alias, enabled) ${body}`,
  )(...Object.values(dependencies));

  for (const lastSyncError of ["APPLE_ALIAS_NOT_FOUND", "APPLE_ALIAS_INACTIVE"]) {
    await toggle({ id: 7, address: "archived@icloud.com", enabled: false, lastSyncError }, true);
  }
  for (const target of ["row", "alias"]) {
    assert.ok(source.includes(`:disabled="isAliasActionBusy(${target}) || Boolean(appleAliasDirectoryStatus(${target}))"`));
  }
  assert.doesNotMatch(functionBody(source, "async function copyAliasCredentials"), /appleAliasDirectoryStatus/);
});
