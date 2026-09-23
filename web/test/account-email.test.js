import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { computed, ref } from "vue";
import * as api from "../src/api/admin.js";
import { createLatestRequestGate } from "../src/utils/asyncState.js";

const source = await readFile(new URL("../src/components/AppleAccountEmailDialog.vue", import.meta.url), "utf8");
const script = source.match(/<script setup>([\s\S]*?)<\/script>/)[1].replace(/^import [\s\S]*? from "[^"]+";\s*/gm, "");
const profile = { can_add: true, emails: [
  { id: 1, address: "owner@example.com", removable: false },
  { id: 3, address: "alternate@example.com", removable: true },
] };

function harness(overrides = {}, action = "add", email = "") {
  const events = [];
  let unmount;
  const unexpected = async () => assert.fail("unexpected mutation");
  const deps = {
    computed, ref, createLatestRequestGate,
    defineProps: () => ({ accountId: 7, csrfToken: "csrf", action, email }),
    defineEmits: () => (...args) => events.push(args),
    onMounted: () => {}, onBeforeUnmount: (fn) => { unmount = fn; },
    getAccountEmails: async () => profile,
    beginAccountEmail: unexpected, deleteAccountEmail: unexpected,
    loginEmailAccount: unexpected, verifyEmailAccountLogin: unexpected, verifyAccountEmail: unexpected,
    ...overrides,
  };
  const state = new Function(...Object.keys(deps), `${script}; return {prepare, submit, closeDialog, restartEmail, step, busy, error, address, password, code, challengeId};`)(...Object.values(deps));
  return { ...state, events, unmount: () => unmount() };
}

test("account email API scopes writes and sends credentials/codes only in CSRF-protected bodies", async (t) => {
  const calls = [];
  t.mock.method(globalThis, "fetch", async (url, options) => {
    calls.push({ url, options });
    return new Response(JSON.stringify({ data: { status: "complete" } }), { headers: { "Content-Type": "application/json" } });
  });
  await api.getAccountEmails(7);
  await api.loginEmailAccount(7, "secret", "csrf");
  await api.verifyEmailAccountLogin(7, "auth-id", "123456", "csrf");
  await api.beginAccountEmail(7, "new@example.com", "csrf");
  await api.verifyAccountEmail(7, "email-id", "654321", "csrf");
  await api.deleteAccountEmail(7, "new@example.com", "csrf");
  assert.deepEqual(calls.map(({url, options}) => [options.method, url.replace("/admin/api/v1/accounts/7/forwarding/", "")]), [
    ["GET", "emails"], ["POST", "email-auth"], ["POST", "email-auth/verify"],
    ["POST", "emails"], ["POST", "emails/verify"], ["DELETE", "emails"],
  ]);
  for (const {url, options} of calls.slice(1)) {
    assert.equal(options.headers.get("X-CSRF-Token"), "csrf");
    assert.doesNotMatch(url, /secret|123456|654321|example/);
  }
  assert.deepEqual(JSON.parse(calls[5].options.body), { address: "new@example.com" });
});

test("adding completes only after the mailbox code is verified", async () => {
  const calls = [];
  const dialog = harness({
    beginAccountEmail: async (...args) => { calls.push(args); return { status: "email_verification_required", challenge_id: "email-id", address: "new@example.com" }; },
    verifyAccountEmail: async (...args) => { calls.push(args); return { status: "complete", address: "new@example.com" }; },
  });
  await dialog.prepare();
  dialog.address.value = "new@example.com";
  await dialog.submit();
  assert.equal(dialog.step.value, "email-code");
  assert.deepEqual(dialog.events, []);
  dialog.code.value = "123";
  await dialog.submit();
  assert.equal(calls.length, 1);
  dialog.code.value = "654321";
  await dialog.submit();
  assert.deepEqual(calls, [[7, "new@example.com", "csrf"], [7, "email-id", "654321", "csrf"]]);
  assert.deepEqual(dialog.events, [["changed", { action: "add", address: "new@example.com" }]]);
});

test("portal login and device verification return to the email action without auto-submitting it", async () => {
  let authenticated = false;
  const dialog = harness({
    getAccountEmails: async () => { if (!authenticated) throw { code: "APPLE_EMAIL_ACCOUNT_AUTH_REQUIRED" }; return profile; },
    loginEmailAccount: async () => ({ status: "verification_required", challenge_id: "auth-id" }),
    verifyEmailAccountLogin: async () => { authenticated = true; return { status: "authenticated" }; },
  });
  await dialog.prepare();
  assert.equal(dialog.step.value, "login");
  dialog.password.value = "secret";
  await dialog.submit();
  assert.equal(dialog.password.value, "");
  assert.equal(dialog.step.value, "apple-code");
  dialog.code.value = "123456";
  await dialog.submit();
  assert.equal(dialog.step.value, "address");
  assert.deepEqual(dialog.events, []);
});

test("deletion requires a removable email and an explicit confirmation", async () => {
  let deletes = 0;
  const dialog = harness({ deleteAccountEmail: async () => { deletes++; return { status: "complete", address: "alternate@example.com" }; } }, "delete", "alternate@example.com");
  await dialog.prepare();
  assert.equal(dialog.step.value, "delete");
  assert.equal(deletes, 0);
  await dialog.submit();
  assert.equal(deletes, 1);
  assert.equal(dialog.events[0][0], "changed");
  const primary = harness({}, "delete", "owner@example.com");
  await primary.prepare();
  assert.equal(primary.step.value, "blocked");
  await primary.submit();
  const last = harness({ getAccountEmails: async () => ({ ...profile, emails: [profile.emails[1]] }) }, "delete", "alternate@example.com");
  await last.prepare();
  assert.equal(last.step.value, "blocked");
});

test("invalid mailbox codes may retry, while uncertain writes require rechecking", async () => {
  let calls = 0;
  const dialog = harness({
    beginAccountEmail: async () => ({ status: "email_verification_required", challenge_id: "email-id", address: "new@example.com" }),
    verifyAccountEmail: async () => { calls++; throw { code: calls === 1 ? "APPLE_EMAIL_CODE_INVALID" : "NETWORK_ERROR" }; },
  });
  await dialog.prepare(); dialog.address.value = "new@example.com"; await dialog.submit();
  dialog.code.value = "654321"; await dialog.submit();
  assert.equal(dialog.step.value, "email-code");
  await dialog.submit();
  assert.equal(dialog.step.value, "blocked");
  await dialog.submit();
  assert.equal(calls, 2);
  assert.deepEqual(dialog.events, []);
});

test("pending mutations cannot be duplicated and late replies cannot update another account", async () => {
  let resolve;
  let calls = 0;
  const dialog = harness({ deleteAccountEmail: () => { calls++; return new Promise((done) => { resolve = done; }); } }, "delete", "alternate@example.com");
  await dialog.prepare();
  const request = dialog.submit();
  await dialog.submit(); dialog.closeDialog();
  assert.equal(calls, 1);
  assert.deepEqual(dialog.events, []);
  dialog.unmount();
  resolve({ status: "complete" });
  await request;
  assert.deepEqual(dialog.events, []);
});
