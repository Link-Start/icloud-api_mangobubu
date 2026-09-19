import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { appleAliasDirectoryStatus } from "../src/utils/appleAliasState.js";

const syncStatusPath = new URL(
  "../src/components/SyncStatus.vue",
  import.meta.url,
);
const accountDetailPath = new URL(
  "../src/views/AccountDetailView.vue",
  import.meta.url,
);
const accountsViewPath = new URL(
  "../src/views/AccountsView.vue",
  import.meta.url,
);

test("shared sync status renders manual and automatic progress", async () => {
  const source = await readFile(syncStatusPath, "utf8");

  assert.match(source, /syncProgressPresentation\(props\.item\.syncProgress\)/);
  assert.match(source, /v-if="progress\.active && !appleDirectoryStatus"/);
  assert.match(source, /<el-progress/);
  assert.match(source, /:indeterminate="progress\.indeterminate"/);
  assert.match(
    source,
    /:aria-hidden="progress\.indeterminate \? 'true' : undefined"/,
  );
  assert.match(source, /progress\.stageLabel/);
  assert.match(source, /progress\.percentage/);
});

test("Apple directory states distinguish local removal from retained inactive addresses", async () => {
  const source = await readFile(syncStatusPath, "utf8");
  const statusCallback = source.match(/const status = computed\(\(\) => (\{[\s\S]*?\n\})\);/);
  assert.ok(statusCallback, "missing status computation");
  for (const [code, label] of [
    ["APPLE_ALIAS_NOT_FOUND", "Apple 已不存在"],
    ["APPLE_ALIAS_INACTIVE", "Apple 已停用"],
  ]) {
    for (const enabled of [false, true]) {
      const alias = { address: "archived@icloud.com", enabled, lastSyncStatus: "error", lastSyncError: code };
      const directoryStatus = appleAliasDirectoryStatus(alias);
      const status = Function(
        "props", "appleDirectoryStatus", "progress",
        `return function () ${statusCallback[1]}`,
      )({ item: alias }, { value: directoryStatus }, { value: { active: false } })();
      assert.equal(status.label, label);
      if (code === "APPLE_ALIAS_NOT_FOUND") {
        assert.match(directoryStatus.description, /同步完整 Apple 目录.*自动移除本地地址及关联数据/);
        assert.doesNotMatch(directoryStatus.description, /归档保留/);
      } else {
        assert.match(directoryStatus.description, /本地归档保留/);
        assert.match(directoryStatus.description, /同步目录.*可手动启用/);
      }
    }
    assert.equal(appleAliasDirectoryStatus({ enabled: false, lastSyncError: code }), null);
  }
  for (const code of ["", "IMAP_FETCH_FAILED", "APPLE_ALIAS_CONFIRMATION_PENDING", "toString"]) {
    assert.equal(appleAliasDirectoryStatus({ address: "alias@icloud.com", lastSyncError: code }), null);
  }
  assert.match(source, /v-if="details && appleDirectoryStatus"/);
  assert.match(source, /props\.details &&\s*!appleDirectoryStatus\.value/);
});

test("account list and detail share server-driven sync progress", async () => {
  const [detailSource, listSource] = await Promise.all([
    readFile(accountDetailPath, "utf8"),
    readFile(accountsViewPath, "utf8"),
  ]);

  assert.match(detailSource, /const syncActive = computed\(/);
  assert.match(detailSource, /account\.value\?\.syncProgress\?\.active/);
  assert.match(detailSource, /:loading="syncLoading \|\| syncActive"/);
  assert.match(
    detailSource,
    /if \(syncLoading\.value \|\| syncActive\.value \|\| randomAliasLoading\.value\) return/,
  );
  assert.ok(
    (listSource.match(/<SyncStatus :item=/g) || []).length >= 2,
    "desktop and mobile account lists must use the shared sync status",
  );
});

test("existing sync error summaries open the reusable full log dialog", async () => {
  const [statusSource, detailSource] = await Promise.all([
    readFile(syncStatusPath, "utf8"),
    readFile(accountDetailPath, "utf8"),
  ]);

  assert.match(
    statusSource,
    /错误：\{\{ compactRunes\(item\.lastSyncError\) \}\}[\s\S]*<SyncErrorLogDialog/,
  );
  assert.match(
    detailSource,
    /\{\{ account\.lastSyncError \}\}[\s\S]*<SyncErrorLogDialog/,
  );
  assert.match(
    detailSource,
    /account\.lastSyncErrorLog \|\| account\.lastSyncError/,
  );
});
