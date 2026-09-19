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

test("original information follows one failed entry and is included in copied creation flows", async (context) => {
  const source = await readFile(new URL("../src/components/RuntimeLogDetailDialog.vue", import.meta.url), "utf8");
  const scope = effectScope();
  context.after(() => scope.stop());
  const started = runtimeLogs.normalizeRuntimeLog({
    id: 1,
    account_id: 12,
    time: "2026-09-19T13:20:40Z",
    attributes: {
      auto_create_run_id: "auto-run-1",
      auto_create_event: "run_started",
      auto_create_stage: "preparing",
    },
  });
  const failed = runtimeLogs.normalizeRuntimeLog({
    id: 2,
    account_id: 12,
    level: "error",
    time: "2026-09-19T13:20:48Z",
    message: "自动创建隐私邮箱失败",
    attributes: {
      auto_create_run_id: "auto-run-1",
      auto_create_event: "run_failed",
      auto_create_stage: "failed",
      auto_create_kind: "new",
      http_status: 503,
      operation: "reconcile alias directory",
      apple_response_excerpt: '{"success":false,"error":{"code":"-27577","message":"<script>alert(1)</script>"},"count":9007199254740993}',
      apple_response_format: "json",
      apple_response_bytes: 312,
      apple_response_truncated: false,
      apple_response_operation: "reserve Hide My Email alias",
      apple_response_http_status: 200,
      apple_response_service_code: "-27577",
    },
  });
  const anotherFailed = runtimeLogs.normalizeRuntimeLog({
    ...failed,
    id: 3,
    appleResponseExcerpt: '{"error":{"code":"-2"}}',
    appleResponseServiceCode: "-2",
  });
  const props = reactive({
    modelValue: true,
    log: started,
    flowLogs: [started, failed, anotherFailed],
    flowLoading: false,
    flowError: null,
    accountLabel: "owner@icloud.com",
  });
  const detail = scope.run(() => evaluateSetup(source, {
    defineProps: () => props,
    defineEmits: () => () => {},
    formatTime,
  }, [
    "canViewOriginalInformation", "isOriginalInformationOpen", "toggleOriginalInformation",
    "originalInformationNotice", "originalInformationText", "fullLogText",
  ]));

  assert.equal(detail.canViewOriginalInformation(started), false);
  assert.equal(detail.canViewOriginalInformation(failed), true);
  assert.equal(detail.isOriginalInformationOpen(failed), false);
  assert.match(detail.fullLogText.value, /原始信息:\n敏感字段已脱敏。/);
  assert.match(detail.fullLogText.value, /响应对应 Apple 操作: reserve Hide My Email alias/);
  assert.match(detail.fullLogText.value, /响应 HTTP 状态: 200/);
  assert.match(detail.fullLogText.value, /Apple 业务码: -27577/);
  assert.match(detail.fullLogText.value, /"code":"-27577"/);
  assert.doesNotMatch(detail.fullLogText.value, /apple_response_excerpt/);

  detail.toggleOriginalInformation(failed);
  assert.equal(detail.isOriginalInformationOpen(failed), true);
  assert.equal(detail.isOriginalInformationOpen(anotherFailed), false);
  const originalText = detail.originalInformationText(failed);
  assert.match(originalText, /已读取响应长度: 312 字节/);
  assert.match(originalText, /<script>alert\(1\)<\/script>/);
  assert.ok(originalText.endsWith(failed.appleResponseExcerpt));
  assert.doesNotMatch(originalText, /503|reconcile alias directory|已截断/);
  assert.match(source, /<pre v-if="hasSavedAppleResponse\(entry\)">\{\{ originalInformationText\(entry\) \}\}<\/pre>/);
  assert.doesNotMatch(source, /v-html|innerHTML/);

  detail.toggleOriginalInformation(anotherFailed);
  assert.equal(detail.isOriginalInformationOpen(failed), false);
  assert.equal(detail.isOriginalInformationOpen(anotherFailed), true);
  detail.toggleOriginalInformation(anotherFailed);
  assert.equal(detail.isOriginalInformationOpen(anotherFailed), false);

  detail.toggleOriginalInformation(failed);
  props.log = failed;
  await nextTick();
  assert.equal(detail.isOriginalInformationOpen(failed), false);
  detail.toggleOriginalInformation(failed);
  props.modelValue = false;
  await nextTick();
  assert.equal(detail.isOriginalInformationOpen(failed), false);

  props.modelValue = true;
  props.log = runtimeLogs.normalizeRuntimeLog({ ...failed, accountId: 13, autoCreateRunId: "auto-run-2" });
  props.flowLogs = [props.log];
  await nextTick();
  assert.equal(detail.isOriginalInformationOpen(props.log), false);
  detail.toggleOriginalInformation(props.log);
  assert.equal(detail.isOriginalInformationOpen(props.log), true);
  assert.equal(detail.isOriginalInformationOpen(failed), false);
});

test("single snapshots and historical creation failures explain missing, omitted, empty and truncated responses", async (context) => {
  const source = await readFile(new URL("../src/components/RuntimeLogDetailDialog.vue", import.meta.url), "utf8");
  const scope = effectScope();
  context.after(() => scope.stop());
  const props = reactive({
    modelValue: true,
    log: runtimeLogs.normalizeRuntimeLog({ id: 7, level: "error", message: "Apple 请求失败" }),
    flowLogs: [],
    flowLoading: false,
    flowError: null,
    accountLabel: "",
  });
  const detail = scope.run(() => evaluateSetup(source, {
    defineProps: () => props,
    defineEmits: () => () => {},
    formatTime,
  }, [
    "canViewOriginalInformation", "hasSavedAppleResponse", "isOriginalInformationOpen",
    "toggleOriginalInformation", "originalInformationNotice", "originalInformationText", "fullLogText",
  ]));

  assert.equal(detail.canViewOriginalInformation(props.log), false);
  assert.doesNotMatch(detail.fullLogText.value, /原始信息:|新产生的失败日志会记录/);
  props.log = runtimeLogs.normalizeRuntimeLog({
    id: 7,
    level: "error",
    message: "Apple 请求失败",
    auto_create_run_id: "historical-auto-run",
  });
  await nextTick();
  assert.equal(detail.canViewOriginalInformation(props.log), true);
  assert.equal(detail.hasSavedAppleResponse(props.log), false);
  assert.equal(detail.originalInformationText(props.log), "");
  assert.equal(detail.originalInformationNotice(props.log), "该日志未保存 Apple 原始响应；升级后新产生的隐私邮箱接口失败日志会记录可用的脱敏响应。");
  assert.match(detail.fullLogText.value, /该日志未保存 Apple 原始响应/);
  assert.doesNotMatch(detail.fullLogText.value, /\{\}|Apple 返回了空响应正文/);
  detail.toggleOriginalInformation(props.log);
  assert.equal(detail.isOriginalInformationOpen(props.log), true);

  const response = (attributes) => runtimeLogs.normalizeRuntimeLog({
    id: 8,
    level: "error",
    attributes,
  });
  props.log = response({
    apple_response_excerpt: '{"error":{"code":"-27577","message":"很长的响应',
    apple_response_format: "json",
    apple_response_bytes: 12345,
    apple_response_truncated: true,
    apple_response_service_code: "-27577",
  });
  await nextTick();
  assert.equal(detail.canViewOriginalInformation(props.log), true);
  assert.equal(detail.isOriginalInformationOpen(props.log), false);
  assert.match(detail.originalInformationText(props.log), /已读取响应长度: 12345 字节/);
  assert.match(detail.originalInformationText(props.log), /响应内容已截断/);
  assert.ok(detail.originalInformationText(props.log).endsWith(props.log.appleResponseExcerpt));
  assert.match(detail.fullLogText.value, /Apple 业务码: -27577/);
  assert.match(source, /<pre v-if="hasSavedAppleResponse\(log\)">\{\{ originalInformationText\(log\) \}\}<\/pre>/);

  props.log = response({
    apple_response_excerpt: "响应正文不是 JSON，原文已省略。",
    apple_response_format: "omitted",
    apple_response_bytes: 71,
    apple_response_truncated: false,
  });
  assert.match(detail.originalInformationText(props.log), /响应正文不是 JSON，原文已省略。/);
  assert.doesNotMatch(detail.originalInformationText(props.log), /已截断/);

  props.log = response({ apple_response_format: "text", apple_response_bytes: 0 });
  assert.equal(detail.hasSavedAppleResponse(props.log), true);
  assert.match(detail.originalInformationText(props.log), /Apple 返回了空响应正文。/);

  props.log = response({
    operation: "reconcile alias directory",
    http_status: 503,
    apple_response_operation: "reserve Hide My Email alias",
    apple_response_http_status: 0,
    apple_response_format: "omitted",
    apple_response_bytes: 0,
    apple_response_excerpt: "请求未收到 HTTP 响应。",
  });
  assert.equal(props.log.appleResponseHttpStatus, null);
  assert.match(detail.originalInformationText(props.log), /响应对应 Apple 操作: reserve Hide My Email alias/);
  assert.match(detail.originalInformationText(props.log), /请求未收到 HTTP 响应。/);
  assert.doesNotMatch(detail.originalInformationText(props.log), /响应 HTTP 状态|503|100|reconcile alias directory/);
});
