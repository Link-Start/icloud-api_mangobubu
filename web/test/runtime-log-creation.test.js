import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { computed, effectScope, nextTick, reactive, ref, watch } from "vue";

import { createLatestRequestGate } from "../src/utils/asyncState.js";
import { formatTime } from "../src/utils/format.js";
import * as pagination from "../src/utils/pagination.js";
import * as runtimeLogs from "../src/utils/runtimeLogs.js";

function evaluateSetup(source, bindings, exposed) {
  const script = source.match(/<script setup>([\s\S]*?)<\/script>/)?.[1];
  assert.ok(script, "component setup script is present");
  const dependencies = {
    computed,
    reactive,
    ref,
    watch,
    onMounted() {},
    onBeforeUnmount() {},
    ...runtimeLogs,
    ...pagination,
    ...bindings,
  };
  const setup = script.replace(/^import[\s\S]*?;\r?\n/gm, "");
  return Function(
    ...Object.keys(dependencies),
    `${setup}\nreturn { ${exposed.join(", ")} };`,
  )(...Object.values(dependencies));
}

test("creation filter follows URLs, refreshes, pages, all-items mode and reset", async (context) => {
  const source = await readFile(new URL("../src/views/LogsView.vue", import.meta.url), "utf8");
  const scope = effectScope();
  context.after(() => scope.stop());
  const route = reactive({
    query: { category: "creation", level: "error", account_id: "12", query: "目录" },
  });
  const routes = [];
  const requests = [];
  let refresh;
  const recordRequest = (options, all = false) => {
    requests.push({ options, all });
    return Promise.resolve(all ? [{ id: 1 }] : { items: [{ id: 1 }], total: 100 });
  };
  const view = scope.run(() => evaluateSetup(source, {
    createLatestRequestGate,
    useRoute: () => route,
    useRouter: () => ({ replace: (value) => routes.push(value) }),
    getRuntimeLogs: (options) => recordRequest(options),
    getAllRuntimeLogs: (options) => recordRequest(options, true),
    createLiveRefresh(callback) {
      refresh = callback;
      return { start() {}, stop() {} };
    },
  }, [
    "filters", "currentPage", "pageSize", "currentFilterKey", "hasActiveFilters",
    "hasAppliedFilters", "keywordDraft", "refreshLatestLogs", "handlePageChange",
    "handlePageSizeChange", "applyFilters", "resetFilters",
  ]));

  assert.equal(view.filters.category, "creation");
  assert.equal(view.hasAppliedFilters.value, true);
  await view.refreshLatestLogs();
  assert.deepEqual(
    Object.fromEntries(Object.entries(requests.at(-1).options).filter(([key]) => key !== "signal")),
    { category: "creation", level: "error", accountId: "12", query: "目录", limit: 20, offset: 0 },
  );
  await refresh();
  assert.equal(requests.at(-1).options.category, "creation");

  view.handlePageChange(2);
  await nextTick();
  assert.equal(requests.at(-1).options.category, "creation");
  assert.equal(requests.at(-1).options.offset, 20);

  view.handlePageSizeChange(pagination.ALL_PAGE_SIZE);
  await nextTick();
  assert.equal(requests.at(-1).all, true);
  assert.equal(requests.at(-1).options.category, "creation");
  assert.equal(requests.at(-1).options.offset, 0);

  const creationKey = view.currentFilterKey.value;
  view.filters.category = "";
  view.applyFilters();
  await nextTick();
  assert.notEqual(view.currentFilterKey.value, creationKey);
  assert.equal(requests.at(-1).options.category, "");
  assert.deepEqual(routes.at(-1), {
    name: "logs", query: { level: "error", account_id: "12", query: "目录" },
  });

  route.query = { category: "creation" };
  await nextTick();
  assert.equal(view.filters.category, "creation");
  assert.equal(view.filters.level, "");
  assert.equal(view.hasActiveFilters.value, true);
  assert.equal(requests.at(-1).options.category, "creation");
  assert.equal(view.currentPage.value, 1);
  view.applyFilters();
  await nextTick();
  assert.deepEqual(routes.at(-1), { name: "logs", query: { category: "creation" } });

  view.resetFilters();
  await nextTick();
  assert.equal(view.filters.category, "");
  assert.equal(view.hasActiveFilters.value, false);
  assert.equal(view.hasAppliedFilters.value, false);
  assert.deepEqual(routes.at(-1), { name: "logs", query: {} });

  route.query = { category: "unsupported" };
  await nextTick();
  assert.equal(view.filters.category, "");
});

test("details and copied flows resolve creation kind from later steps and retain history labels", async (context) => {
  const source = await readFile(new URL("../src/components/RuntimeLogDetailDialog.vue", import.meta.url), "utf8");
  const scope = effectScope();
  context.after(() => scope.stop());
  const entry = (id, kind, stage = "preparing") => runtimeLogs.normalizeRuntimeLog({
    id,
    time: `2026-09-19T09:21:1${id}Z`,
    message: "自动创建隐私邮箱",
    attributes: {
      auto_create_run_id: "auto-run-1",
      auto_create_kind: kind,
      auto_create_stage: stage,
    },
  });
  const selected = entry(1, "undetermined");
  const props = reactive({
    modelValue: true,
    log: selected,
    flowLogs: [selected, entry(2, "new", "reserving"), entry(3, "new", "reconciling")],
    flowLoading: false,
    flowError: null,
    accountLabel: "owner@icloud.com",
  });
  const detail = scope.run(() => evaluateSetup(source, {
    defineProps: () => props,
    defineEmits: () => () => {},
    formatTime,
  }, ["autoCreateKindLabel", "fullLogText"]));

  assert.equal(detail.autoCreateKindLabel.value, "新建隐私邮箱");
  assert.match(detail.fullLogText.value, /创建编号: auto-run-1\n创建类别: 新建隐私邮箱/);
  assert.doesNotMatch(detail.fullLogText.value, /auto_create_kind/);

  props.flowLogs = [selected, entry(2, "reconcile", "reconciling")];
  assert.equal(detail.autoCreateKindLabel.value, "复查待确认地址");
  assert.match(detail.fullLogText.value, /创建类别: 复查待确认地址/);

  props.flowLogs = [];
  assert.equal(detail.autoCreateKindLabel.value, "尚未判定");
  assert.match(detail.fullLogText.value, /创建类别: 尚未判定/);

  props.log = entry(1, undefined);
  assert.equal(detail.autoCreateKindLabel.value, "未记录");
  assert.match(detail.fullLogText.value, /创建类别: 未记录/);

  props.log = runtimeLogs.normalizeRuntimeLog({ id: 9, sync_run_id: "sync-run-1" });
  assert.doesNotMatch(detail.fullLogText.value, /创建类别:/);
});
