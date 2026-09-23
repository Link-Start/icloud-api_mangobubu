import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { computed, ref } from "vue";
import { getForwardingSettings, updateForwardingSettings } from "../src/api/admin.js";
import { createLatestRequestGate } from "../src/utils/asyncState.js";

const source = await readFile(new URL("../src/components/ForwardingSettingsDialog.vue", import.meta.url), "utf8");
const script = source.match(/<script setup>([\s\S]*?)<\/script>/)[1]
  .replace(/^import [\s\S]*? from "[^"]+";\s*/gm, "");
const initialSettings = {
  selectedForwardTo: "owner@icloud.com",
  forwardToEmails: ["owner@icloud.com", "other@example.com"],
};

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

function dialogHarness(overrides = {}) {
  const events = [];
  let unmount;
  const dependencies = {
    computed,
    ref,
    createLatestRequestGate,
    defineProps: () => ({ accountId: 7, csrfToken: "csrf" }),
    defineEmits: () => (...args) => events.push(args),
    onMounted: () => {},
    onBeforeUnmount: (callback) => { unmount = callback; },
    getForwardingSettings: async () => initialSettings,
    updateForwardingSettings: async () => { throw new Error("unexpected save"); },
    ...overrides,
  };
  const setup = new Function(...Object.keys(dependencies), `${script}
    return { loadSettings, saveSettings, closeDialog, loading, saving, loaded, error, emails, currentTarget, selectedTarget, canSave, openEmailAction, finishEmailChange, emailAction, notice };
  `);
  return { ...setup(...Object.values(dependencies)), events, unmount: () => unmount() };
}

test("forwarding API uses the account scope, single target, and CSRF token", async (t) => {
  const fetchCalls = [];
  t.mock.method(globalThis, "fetch", async (url, options) => {
    fetchCalls.push({ url, options });
    return new Response(JSON.stringify({ data: {
      selected_forward_to: "other@example.com",
      forward_to_emails: initialSettings.forwardToEmails,
    } }), { headers: { "Content-Type": "application/json" } });
  });
  assert.equal((await getForwardingSettings(7)).selectedForwardTo, "other@example.com");
  await updateForwardingSettings(7, "other@example.com", "csrf");
  assert.equal(fetchCalls[0].url, "/admin/api/v1/accounts/7/forwarding");
  assert.equal(fetchCalls[0].options.method, "GET");
  assert.equal(fetchCalls[1].options.method, "PUT");
  assert.equal(fetchCalls[1].options.headers.get("X-CSRF-Token"), "csrf");
  assert.deepEqual(JSON.parse(fetchCalls[1].options.body), { forward_to_email: "other@example.com" });
});

test("dialog preselects the current target and permits only one available address", async () => {
  const dialog = dialogHarness();
  assert.equal(dialog.canSave.value, false);
  await dialog.loadSettings();
  assert.equal(dialog.selectedTarget.value, "owner@icloud.com");
  assert.equal(dialog.canSave.value, true);
  dialog.selectedTarget.value = "unavailable@example.com";
  assert.equal(dialog.canSave.value, false);
  dialog.selectedTarget.value = "other@example.com";
  assert.equal(dialog.canSave.value, true);
});

test("email controls protect the last forwarding address and refresh choices after add/remove", async () => {
  let calls = 0;
  let settings = { selectedForwardTo: "owner@icloud.com", forwardToEmails: ["owner@icloud.com"] };
  const dialog = dialogHarness({ getForwardingSettings: async () => { calls++; return settings; } });
  await dialog.loadSettings();
  dialog.openEmailAction("delete", "owner@icloud.com");
  assert.equal(dialog.emailAction.value, null);
  dialog.openEmailAction("add");
  assert.equal(dialog.canSave.value, false);
  settings = initialSettings;
  await dialog.finishEmailChange({ action: "add", address: "other@example.com" });
  assert.equal(calls, 2);
  assert.deepEqual(dialog.emails.value, initialSettings.forwardToEmails);
  dialog.openEmailAction("delete", "other@example.com");
  assert.equal(dialog.emailAction.value.email, "other@example.com");
  settings = { selectedForwardTo: "owner@icloud.com", forwardToEmails: ["owner@icloud.com"] };
  await dialog.finishEmailChange({ action: "delete", address: "other@example.com" });
  assert.equal(calls, 3);
  assert.deepEqual(dialog.emails.value, ["owner@icloud.com"]);
});

test("empty choices and an unavailable current target require a valid selection", async () => {
  for (const settings of [
    { selectedForwardTo: "", forwardToEmails: [] },
    { selectedForwardTo: "removed@example.com", forwardToEmails: ["other@example.com"] },
  ]) {
    const dialog = dialogHarness({ getForwardingSettings: async () => settings });
    await dialog.loadSettings();
    assert.equal(dialog.selectedTarget.value, "");
    assert.equal(dialog.canSave.value, false);
  }
});

test("saving blocks duplicate submissions and closing until Apple confirms the result", async () => {
  const pending = deferred();
  const calls = [];
  const dialog = dialogHarness({ updateForwardingSettings: (...args) => {
    calls.push(args);
    return pending.promise;
  } });
  await dialog.loadSettings();
  dialog.selectedTarget.value = "other@example.com";
  const save = dialog.saveSettings();
  await dialog.saveSettings();
  dialog.closeDialog();
  assert.deepEqual(calls, [[7, "other@example.com", "csrf"]]);
  assert.equal(dialog.saving.value, true);
  assert.deepEqual(dialog.events, []);
  const result = { ...initialSettings, selectedForwardTo: "other@example.com" };
  pending.resolve(result);
  await save;
  assert.deepEqual(dialog.events, [["saved", result]]);
});

test("failed saves require refreshing current settings before another submission", async () => {
  let calls = 0;
  const failure = Object.assign(new Error("choice unavailable"), { code: "APPLE_FORWARDING_TARGET_INVALID" });
  const dialog = dialogHarness({ updateForwardingSettings: async () => {
    calls++;
    throw failure;
  } });
  await dialog.loadSettings();
  await dialog.saveSettings();
  await dialog.saveSettings();
  assert.equal(calls, 1);
  assert.equal(dialog.canSave.value, false);
  assert.equal(dialog.error.value, failure);
  await dialog.loadSettings();
  assert.equal(dialog.error.value, null);
  assert.equal(dialog.canSave.value, true);
});

test("late reads and saves cannot affect a closed dialog or a different account", async () => {
  const read = deferred();
  const dialog = dialogHarness({ getForwardingSettings: () => read.promise });
  const loading = dialog.loadSettings();
  dialog.closeDialog();
  read.resolve(initialSettings);
  await loading;
  assert.deepEqual(dialog.emails.value, []);
  assert.deepEqual(dialog.events, [["close"]]);

  const mutation = deferred();
  const savingDialog = dialogHarness({ updateForwardingSettings: () => mutation.promise });
  await savingDialog.loadSettings();
  const saving = savingDialog.saveSettings();
  savingDialog.unmount();
  mutation.resolve(initialSettings);
  await saving;
  assert.deepEqual(savingDialog.events, []);
});

test("expired sessions hand control to the existing Apple login flow", async () => {
  const failure = { code: "APPLE_SESSION_EXPIRED" };
  const dialog = dialogHarness({ getForwardingSettings: async () => { throw failure; } });
  await dialog.loadSettings();
  assert.deepEqual(dialog.events, [["auth-required", failure]]);
  assert.equal(dialog.canSave.value, false);
});

test("successful Apple authentication reopens forwarding without starting alias synchronization", async () => {
  const detailSource = await readFile(new URL("../src/views/AccountDetailView.vue", import.meta.url), "utf8");
  const definition = detailSource.slice(
    detailSource.indexOf("async function finishAppleAuthentication("),
    detailSource.indexOf("async function submitAppleLogin("),
  );
  const forwardingVisible = ref(false);
  const appleAuthVisible = ref(true);
  const dependencies = {
    isCurrentAccount: (id) => id === 7,
    appleSession: ref(null),
    mergedAppleSession: (result) => result,
    resumeAliasSyncAfterAuth: false,
    resumeAutoCreationAfterAuth: false,
    resumeForwardingAfterAuth: true,
    appleAuthVisible,
    forwardingVisible,
    resetAppleAuthForm: () => {},
    successMessage: () => {},
    nextTick: async () => {},
    performSetAutoCreation: () => assert.fail("unexpected automatic creation"),
    performAliasesSync: () => assert.fail("unexpected directory synchronization"),
  };
  const finish = new Function(...Object.keys(dependencies), `${definition}; return finishAppleAuthentication;`)(...Object.values(dependencies));
  await finish({ status: "authenticated" }, 7);
  assert.equal(appleAuthVisible.value, false);
  assert.equal(forwardingVisible.value, true);
  forwardingVisible.value = false;
  await finish({ status: "authenticated" }, 7);
  assert.equal(forwardingVisible.value, false, "authentication continuation should be consumed once");
});
