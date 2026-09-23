<template>
  <el-dialog
    :model-value="true"
    title="转发设置"
    width="min(520px, calc(100vw - 28px))"
    :close-on-click-modal="false"
    :close-on-press-escape="!saving"
    :show-close="!saving"
    @update:model-value="closeDialog"
  >
    <p class="forwarding-description">
      选择此 Apple 账户的隐私邮箱转发地址。请确保主号邮箱及 IMAP 收件配置与转发目标对应。
    </p>
    <RequestAlert v-if="error" :error="error" />
    <el-skeleton v-if="loading" :rows="3" animated />
    <template v-else-if="loaded">
      <p class="forwarding-current">当前转发至：{{ currentTarget || "尚未设置" }}</p>
      <el-radio-group
        v-if="emails.length"
        v-model="selectedTarget"
        class="forwarding-options"
        aria-label="转发目标邮箱"
        :disabled="saving"
      >
        <el-radio v-for="email in emails" :key="email" :value="email" border>
          {{ email }}
        </el-radio>
      </el-radio-group>
      <el-empty v-else description="当前 Apple 账户没有可设置转发的邮箱" :image-size="64" />
    </template>
    <template #footer>
      <div class="dialog-actions">
        <el-button :disabled="loading || saving" @click="loadSettings">重新获取</el-button>
        <el-button :disabled="saving" @click="closeDialog">取消</el-button>
        <el-button type="primary" :loading="saving" :disabled="!canSave" @click="saveSettings">
          确定
        </el-button>
      </div>
    </template>
  </el-dialog>
</template>

<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { getForwardingSettings, updateForwardingSettings } from "../api/admin.js";
import RequestAlert from "./RequestAlert.vue";
import { createLatestRequestGate } from "../utils/asyncState.js";

const props = defineProps({
  accountId: { type: [Number, String], required: true },
  csrfToken: { type: String, required: true },
});
const emit = defineEmits(["close", "saved", "auth-required"]);
const loading = ref(false);
const saving = ref(false);
const loaded = ref(false);
const error = ref(null);
const emails = ref([]);
const currentTarget = ref("");
const selectedTarget = ref("");
const requestGate = createLatestRequestGate();
const canSave = computed(
  () => loaded.value && !loading.value && !saving.value && emails.value.includes(selectedTarget.value),
);

function handleError(caught) {
  if (["APPLE_LOGIN_REQUIRED", "APPLE_AUTH_REQUIRED", "APPLE_SESSION_EXPIRED"].includes(caught?.code)) {
    emit("auth-required", caught);
    return;
  }
  error.value = caught;
}

async function loadSettings() {
  if (loading.value || saving.value) return;
  const ticket = requestGate.begin(props.accountId);
  loading.value = true;
  loaded.value = false;
  error.value = null;
  emails.value = [];
  selectedTarget.value = "";
  try {
    const settings = await getForwardingSettings(props.accountId);
    if (!requestGate.isCurrent(ticket, props.accountId)) return;
    emails.value = settings.forwardToEmails;
    currentTarget.value = settings.selectedForwardTo;
    selectedTarget.value = emails.value.includes(currentTarget.value) ? currentTarget.value : "";
    loaded.value = true;
  } catch (caught) {
    if (requestGate.isCurrent(ticket, props.accountId)) handleError(caught);
  } finally {
    if (requestGate.isCurrent(ticket, props.accountId)) loading.value = false;
  }
}

async function saveSettings() {
  if (!canSave.value) return;
  const ticket = requestGate.begin(props.accountId);
  const target = selectedTarget.value;
  saving.value = true;
  error.value = null;
  try {
    const settings = await updateForwardingSettings(props.accountId, target, props.csrfToken);
    if (!requestGate.isCurrent(ticket, props.accountId)) return;
    emit("saved", settings);
  } catch (caught) {
    if (!requestGate.isCurrent(ticket, props.accountId)) return;
    // A rejected choice or a lost response requires a fresh list before the
    // next submission; never silently resubmit the previous mutation.
    loaded.value = false;
    handleError(caught);
  } finally {
    if (requestGate.isCurrent(ticket, props.accountId)) saving.value = false;
  }
}

function closeDialog() {
  if (saving.value) return;
  requestGate.invalidate();
  emit("close");
}

onMounted(loadSettings);
onBeforeUnmount(() => requestGate.deactivate());
</script>

<style scoped>
.forwarding-description,
.forwarding-current {
  margin: 0 0 16px;
  color: var(--text-secondary);
  line-height: 1.6;
  overflow-wrap: anywhere;
}

.forwarding-options {
  display: flex;
  flex-direction: column;
  align-items: stretch;
  gap: 10px;
  width: 100%;
}

.forwarding-options :deep(.el-radio) {
  height: auto;
  min-height: 40px;
  margin: 0;
  padding: 10px 12px;
}

.forwarding-options :deep(.el-radio__label) {
  min-width: 0;
  white-space: normal;
  overflow-wrap: anywhere;
}
</style>
