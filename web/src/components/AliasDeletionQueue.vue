<template>
  <section
    v-if="state.jobs?.length || state.recovering || state.submitting || state.uncertain"
    class="data-panel alias-deletion-progress"
    aria-labelledby="alias-deletion-title"
  >
    <div class="alias-deletion-progress__header">
      <strong id="alias-deletion-title">Apple 删除队列</strong>
      <span class="alias-deletion-progress__hint">
        {{ state.jobs?.filter(isAliasDeletionJobActive).length || 0 }} 个进行中任务
      </span>
      <el-button :loading="state.checking" :disabled="state.submitting || state.clearing || busy" @click="$emit('refresh')">
        刷新任务状态
      </el-button>
      <el-button
        :loading="state.clearing"
        :disabled="busy || state.submitting || state.recovering || !!state.operationId || !state.jobs?.some(job => job.status === 'completed')"
        @click="$emit('clear-completed')"
      >清空已完成任务</el-button>
    </div>
    <p class="alias-deletion-progress__hint">
      不同主号同时执行；同一主号按提交顺序处理，每 60 分钟最多删除 200 个，剩余自动等待。
      排队期间同步和创建照常进行。离开页面或重启服务后，队列会继续处理。
    </p>
    <p v-if="state.recovering" role="status">正在恢复当前管理员的任务列表。</p>
    <p v-if="state.submitting" role="status">正在添加删除任务…</p>
    <p v-if="state.uncertain" class="alias-deletion-progress__hint" role="status">
      <template v-if="state.operationId">
        {{ state.unmatched ? "尚未查到本次提交对应的任务。" : "本次提交结果待确认。" }}
        正在查询确认，确认后可继续添加任务；已添加的队列照常执行。
      </template>
      <template v-else>任务进度暂未更新，保留最近已知进度，稍后自动重试查询。</template>
      <span v-if="state.error?.requestId">请求编号：{{ state.error.requestId }}</span>
    </p>
    <p v-if="state.operationId" class="alias-deletion-progress__hint">待确认任务：{{ state.operationId }}</p>
    <div v-if="state.unmatched" class="alias-deletion-progress__hint">
      如需重新选择，请先在主号详情刷新 Apple 目录确认实际状态。
      <el-button :disabled="state.checking || busy" @click="$emit('acknowledge')">
        已刷新 Apple 目录并核对结果
      </el-button>
    </div>

    <article v-for="job in state.jobs" :key="job.jobId" class="alias-deletion-progress__job">
      <div class="alias-deletion-progress__header">
        <strong>{{ formatTime(job.createdAt) }} · {{ job.requested }} 个邮箱</strong>
        <el-tag :type="aliasDeletionJobType(job)">{{ aliasDeletionJobLabel(job) }}</el-tag>
        <el-button
          v-if="isAliasDeletionJobActive(job)"
          :loading="state.cancelling?.includes(job.jobId) || cancellingIds.includes(job.jobId)"
          :disabled="job.cancelRequested"
          @click="$emit('cancel', job.jobId)"
        >取消剩余任务</el-button>
      </div>
      <p role="status" aria-live="polite" aria-atomic="true">
        已处理 {{ job.processed }} / {{ job.requested }}；成功删除 {{ job.deleted }}；
        待执行 {{ aliasDeletionPending(job) }}；失败 {{ job.failed }}
        <span v-if="job.cancelled">；已取消 {{ job.cancelled }}</span>
        <span v-if="job.deferred">（旧任务未执行 {{ job.deferred }}）</span>
      </p>
      <el-progress :percentage="aliasDeletionPercentage(job)" :show-text="false" aria-label="Apple 删除任务进度" />
      <div v-if="job.accounts?.length" class="alias-deletion-progress__accounts">
        <div v-for="account in job.accounts" :key="account.accountId" class="alias-deletion-progress__account">
          <div class="alias-deletion-progress__header">
            <strong>{{ account.accountEmail || `主号 ${account.accountId}` }}</strong>
            <el-tag size="small" :type="account.status === 'paused' ? 'warning' : 'info'">
              {{ aliasDeletionAccountLabel(account.status) }}
            </el-tag>
          </div>
          <p>
            已删除 {{ account.deleted }} · 待执行 {{ account.pending }} · 失败 {{ account.failed }}
            <span v-if="account.cancelled"> · 已取消 {{ account.cancelled }}</span>
          </p>
          <p class="alias-deletion-progress__hint">
            最近 60 分钟额度 {{ account.used }} / {{ account.limit }}
            <template v-if="isAliasDeletionJobActive(job) && account.waitReason">
              · {{ aliasDeletionWaitLabel(account.waitReason) }}
              <span v-if="account.retryAt"> · 预计恢复：{{ formatTime(account.retryAt, { seconds: true }) }}</span>
            </template>
          </p>
        </div>
      </div>
      <div v-else-if="isAliasDeletionJobActive(job) && job.waits?.length" class="alias-deletion-progress__waits" role="status">
        <div v-for="(wait, index) in job.waits" :key="index">
          主号 {{ wait.accountId || '待确认' }} · {{ aliasDeletionWaitLabel(wait.reason) }}
          <span v-if="wait.retryAt"> · 预计恢复：{{ formatTime(wait.retryAt, { seconds: true }) }}</span>
        </div>
      </div>
      <p v-if="job.status === 'interrupted'" class="alias-deletion-progress__hint" role="status">
        <template v-if="job.cancelRequested">任务授权已变更，剩余删除已停止。请重新登录并核对 Apple 目录后再操作。</template>
        <template v-else>此旧任务已中断，部分 Apple 结果待确认。请在主号详情刷新 Apple 目录确认后重新选择；系统不会自动重放旧任务的剩余项。</template>
      </p>
      <div v-if="recentFailures(job).length" class="alias-deletion-progress__failures">
        <strong>近期未删除或待确认结果</strong>
        <div v-for="failure in recentFailures(job)" :key="failure.id">
          {{ failure.address || `ID ${failure.id}` }}：{{ formatAliasDeletionResultMessage(failure) }}
        </div>
      </div>
      <details @toggle="setExpanded(job.jobId, $event.target.open)">
        <summary>任务详情<span v-if="job.results?.length">（{{ job.results.length }} 项结果）</span></summary>
        <template v-if="expanded.has(job.jobId)">
          <p class="alias-deletion-progress__hint">任务：{{ job.jobId }} · 更新：{{ formatTime(job.updatedAt) }}</p>
          <ul v-if="job.results?.length" class="alias-deletion-progress__results">
            <li v-for="result in job.results" :key="result.id">
              {{ result.address || `ID ${result.id}` }}：{{ formatAliasDeletionResultMessage(result) }}
            </li>
          </ul>
        </template>
      </details>
    </article>
  </section>
</template>

<script setup>
import { ref } from "vue";
import {
  aliasDeletionAccountLabel,
  aliasDeletionJobLabel,
  aliasDeletionJobType,
  aliasDeletionPending,
  aliasDeletionPercentage,
  aliasDeletionWaitLabel,
  formatAliasDeletionResultMessage,
  isAliasDeletionJobActive,
} from "../utils/aliasDeletionJob.js";
import { formatTime } from "../utils/format.js";

defineProps({
  state: { type: Object, required: true },
  busy: Boolean,
  cancellingIds: { type: Array, default: () => [] },
});
defineEmits(["refresh", "cancel", "acknowledge", "clear-completed"]);

const expanded = ref(new Set());
function setExpanded(jobId, open) {
  const next = new Set(expanded.value);
  if (open) next.add(jobId);
  else next.delete(jobId);
  expanded.value = next;
}
function recentFailures(job) {
  return (job.results || []).filter((result) => !result.deleted).slice(-5).reverse();
}
</script>

<style scoped>
.alias-deletion-progress {
  display: grid;
  flex: 0 0 auto;
  min-width: 0;
  max-height: 44vh;
  gap: 12px;
  padding: 16px;
  overflow: auto;
  overflow-wrap: anywhere;
}
.alias-deletion-progress p { margin: 0; line-height: 1.7; }
.alias-deletion-progress__header { display: flex; flex-wrap: wrap; align-items: center; gap: 10px; }
.alias-deletion-progress__hint { color: var(--text-secondary); font-size: 12px; line-height: 1.6; }
.alias-deletion-progress__job { display: grid; gap: 10px; padding-top: 14px; border-top: 1px solid var(--border); }
.alias-deletion-progress__accounts { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 300px), 1fr)); gap: 10px; }
.alias-deletion-progress__account { display: grid; gap: 6px; padding: 10px; border: 1px solid var(--border); border-radius: 6px; font-size: 13px; }
.alias-deletion-progress__failures, .alias-deletion-progress__waits { display: grid; gap: 6px; font-size: 13px; }
.alias-deletion-progress summary { cursor: pointer; font-size: 13px; }
.alias-deletion-progress__results { max-height: 220px; padding-left: 20px; overflow: auto; font-size: 13px; line-height: 1.7; }
</style>
