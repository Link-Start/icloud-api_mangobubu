import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import {
  AUTH_REQUIRED,
  SESSION_EXPIRED,
  buildLoginRedirect,
  loginNoticeMessage,
  loginNoticeRequiresExplicitLogin,
  loginNoticeType,
} from "../src/utils/authFlow.js";

const loginViewPath = new URL("../src/views/LoginView.vue", import.meta.url);
const routerPath = new URL("../src/router/index.js", import.meta.url);

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

test("fresh unauthenticated visits redirect to login without an expiry notice", () => {
  assert.deepEqual(buildLoginRedirect(AUTH_REQUIRED, "/admin/"), {
    name: "login",
    query: { redirect: "/admin/" },
  });
});

test("only expired sessions receive the expiry notice", () => {
  assert.deepEqual(buildLoginRedirect(SESSION_EXPIRED, "/admin/aliases"), {
    name: "login",
    query: {
      notice: "session_expired",
      redirect: "/admin/aliases",
    },
  });
  assert.deepEqual(buildLoginRedirect("NETWORK_ERROR", "/admin/audit"), {
    name: "login",
    query: {
      notice: "session_error",
      redirect: "/admin/audit",
    },
  });
});

test("starting a new login hides stale session notices", () => {
  assert.equal(
    loginNoticeMessage({ notice: "session_expired" }),
    "登录会话已过期，请重新登录。",
  );
  assert.equal(
    loginNoticeMessage({ notice: "session_expired", dismissed: true }),
    "",
  );
  assert.equal(
    loginNoticeMessage({ notice: "credentials_rotated" }),
    "全部取码令牌与凭据已轮换，请重新登录。",
  );
  assert.equal(
    loginNoticeMessage({ notice: "credentials_changed" }),
    "登录凭据已在其他请求中变化，请重新登录并确认轮换结果。",
  );
  assert.equal(
    loginNoticeMessage({ notice: "rotation_result_invalid" }),
    "令牌轮换已提交，但汇总校验失败；请重新登录后检查操作记录。",
  );
  assert.equal(
    loginNoticeMessage({ notice: "rotation_status_unknown" }),
    "令牌轮换结果暂时无法确认；请重新登录后检查操作记录。",
  );
  assert.equal(
    loginNoticeMessage({ sessionCheckFailed: true, dismissed: true }),
    "",
  );
});

test("successful security changes use a success login notice", () => {
  assert.equal(loginNoticeType("password_changed"), "success");
  assert.equal(loginNoticeType("credentials_rotated"), "success");
  assert.equal(loginNoticeType("session_expired"), "warning");
});

test("rotation notices require an explicit login without restoring a session", () => {
  for (const notice of [
    "credentials_rotated",
    "credentials_changed",
    "rotation_result_invalid",
    "rotation_status_unknown",
  ]) {
    assert.equal(loginNoticeRequiresExplicitLogin(notice), true, notice);
  }
  for (const notice of ["", "session_expired", "password_changed", "session_error"]) {
    assert.equal(loginNoticeRequiresExplicitLogin(notice), false, notice);
  }
});

test("login initialization preserves rotation notices and waits for explicit credentials", async () => {
  const source = await readFile(loginViewPath, "utf8");
  const body = functionBody(source, "async function initialize");
  const events = [];
  const initialize = Function(
    "route",
    "loginNoticeRequiresExplicitLogin",
    "auth",
    "router",
    "redirectTarget",
    "sessionCheckFailed",
    "prepareLogin",
    "initializing",
    `"use strict"; return async function initialize() ${body}`,
  )(
    { query: { notice: "rotation_status_unknown" } },
    loginNoticeRequiresExplicitLogin,
    {
      clearSession: (options) => {
        assert.deepEqual(options, { checked: false });
        events.push("clear-session");
      },
      ensureSession: async () => {
        assert.fail("rotation notice restored the old session");
      },
    },
    { replace: async () => assert.fail("rotation notice redirected to admin") },
    () => ({ name: "accounts" }),
    { value: false },
    async () => events.push("prepare-login"),
    { value: true },
  );

  await initialize();

  assert.deepEqual(events, ["clear-session", "prepare-login"]);
});

test("router never redirects an explicit rotation login notice back to admin", async () => {
  const source = await readFile(routerPath, "utf8");
  assert.match(
    source,
    /to\.name === "login"[\s\S]*auth\.isAuthenticated\.value[\s\S]*!loginNoticeRequiresExplicitLogin\(String\(to\.query\.notice \|\| ""\)\)/,
  );
});
