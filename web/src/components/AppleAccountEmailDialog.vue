<template>
  <el-dialog
    :model-value="true"
    :title="action === 'delete' ? '删除 Apple 邮箱' : '添加 Apple 邮箱'"
    width="min(500px, calc(100vw - 28px))"
    append-to-body
    :close-on-click-modal="false"
    :close-on-press-escape="!busy"
    :show-close="!busy"
    @update:model-value="closeDialog"
  >
    <RequestAlert v-if="error" :error="error" />
    <el-skeleton v-if="step === 'loading'" :rows="3" animated />
    <el-form v-else label-position="top" :disabled="busy" @submit.prevent="submit">
      <template v-if="step === 'login'">
        <p class="email-lead">添加或删除邮箱需要验证当前 Apple 账户。请输入该账户的密码。</p>
        <el-form-item label="Apple 账户密码">
          <el-input v-model="password" type="password" show-password autocomplete="current-password" />
        </el-form-item>
      </template>
      <template v-else-if="step === 'apple-code'">
        <p class="email-lead">请输入受信任的 Apple 设备上显示的验证码。</p>
        <el-form-item label="Apple 双重认证验证码">
          <el-input v-model="code" maxlength="6" inputmode="numeric" autocomplete="one-time-code" />
        </el-form-item>
      </template>
      <template v-else-if="step === 'address'">
        <p class="email-lead">添加一个你能接收验证码的电子邮箱，验证成功后将刷新可转发邮箱列表。</p>
        <el-form-item label="电子邮箱">
          <el-input v-model="address" type="email" maxlength="320" placeholder="name@example.com" autocomplete="email" />
        </el-form-item>
      </template>
      <template v-else-if="step === 'email-code'">
        <p class="email-lead">验证码已发送至 {{ address }}。</p>
        <el-form-item label="邮箱验证码">
          <el-input v-model="code" maxlength="6" inputmode="numeric" autocomplete="one-time-code" />
        </el-form-item>
        <el-button text :disabled="busy" @click="restartEmail">重新输入邮箱或获取验证码</el-button>
      </template>
      <p v-else-if="step === 'delete'" class="email-lead">
        确定从 Apple 账户移除 <strong>{{ email }}</strong>？移除后，该邮箱将不能再作为此账户的转发目标。
      </p>
      <el-button v-if="step === 'blocked'" :disabled="busy" @click="prepare">重新检查</el-button>
    </el-form>
    <template #footer>
      <div class="dialog-actions">
        <el-button :disabled="busy" @click="closeDialog">取消</el-button>
        <el-button
          v-if="!['loading', 'blocked'].includes(step)"
          :type="step === 'delete' ? 'danger' : 'primary'"
          :loading="busy"
          @click="submit"
        >
          {{ actionLabel }}
        </el-button>
      </div>
    </template>
  </el-dialog>
</template>

<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import {
  beginAccountEmail,
  deleteAccountEmail,
  getAccountEmails,
  loginEmailAccount,
  verifyAccountEmail,
  verifyEmailAccountLogin,
} from "../api/admin.js";
import { createLatestRequestGate } from "../utils/asyncState.js";
import RequestAlert from "./RequestAlert.vue";

const props = defineProps({
  accountId: { type: [Number, String], required: true },
  csrfToken: { type: String, required: true },
  action: { type: String, required: true },
  email: { type: String, default: "" },
});
const emit = defineEmits(["close", "changed", "auth-required"]);
const step = ref("loading");
const busy = ref(false);
const error = ref(null);
const password = ref("");
const address = ref("");
const code = ref("");
const challengeId = ref("");
const gate = createLatestRequestGate();
const actionLabel = computed(() => ({
  login: "登录并继续", "apple-code": "验证并继续", address: "获取验证码",
  "email-code": "验证并添加", delete: "确认删除",
})[step.value] || "继续");

function handleError(caught) {
  error.value = caught;
  if (caught?.code === "APPLE_EMAIL_ACCOUNT_AUTH_REQUIRED") {
    step.value = "login";
    challengeId.value = "";
    code.value = "";
  } else if (["APPLE_LOGIN_REQUIRED", "APPLE_AUTH_REQUIRED", "APPLE_SESSION_EXPIRED"].includes(caught?.code)) {
    emit("auth-required", caught);
  } else if (caught?.code === "APPLE_EMAIL_FLOW_EXPIRED") {
    challengeId.value = "";
    code.value = "";
    step.value = step.value === "apple-code" ? "login" : "blocked";
  } else if (step.value === "loading") {
    step.value = "blocked";
  }
}

async function readProfile(ticket) {
  const profile = await getAccountEmails(props.accountId);
  if (!gate.isCurrent(ticket, props.accountId)) return;
  if (props.action === "add") {
    if (!profile.can_add) throw new Error("Apple 当前不允许添加电子邮箱，请在 Apple 账户设置中检查。");
    step.value = "address";
  } else {
    const target = profile.emails?.find((item) => item.address.toLowerCase() === props.email.toLowerCase());
    if (profile.emails?.length <= 1) throw new Error("至少需要保留一个邮箱，不能删除最后一个。");
    if (!target?.removable) throw new Error("此邮箱不能通过备用邮箱接口移除；主邮箱或 Apple 自有别名请在 Apple 账户设置中处理。");
    step.value = "delete";
  }
}

async function prepare() {
  if (busy.value) return;
  const ticket = gate.begin(props.accountId);
  busy.value = true;
  step.value = "loading";
  error.value = null;
  try { await readProfile(ticket); }
  catch (caught) { if (gate.isCurrent(ticket, props.accountId)) handleError(caught); }
  finally { if (gate.isCurrent(ticket, props.accountId)) busy.value = false; }
}

async function submit() {
  if (busy.value || ["loading", "blocked"].includes(step.value)) return;
  if (step.value === "login" && !password.value) { error.value = new Error("请填写 Apple 账户密码。"); return; }
  if (["apple-code", "email-code"].includes(step.value) && !/^\d{6}$/.test(code.value.trim())) {
    error.value = new Error("请输入 6 位数字验证码。"); return;
  }
  if (step.value === "address" && !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(address.value.trim())) {
    error.value = new Error("请填写有效的电子邮箱地址。"); return;
  }
  const ticket = gate.begin(props.accountId);
  const submittedStep = step.value;
  busy.value = true;
  error.value = null;
  try {
    let result;
    if (submittedStep === "login") {
      result = await loginEmailAccount(props.accountId, password.value, props.csrfToken);
    } else if (submittedStep === "apple-code") {
      result = await verifyEmailAccountLogin(props.accountId, challengeId.value, code.value.trim(), props.csrfToken);
    } else if (submittedStep === "address") {
      result = await beginAccountEmail(props.accountId, address.value.trim(), props.csrfToken);
    } else if (submittedStep === "email-code") {
      result = await verifyAccountEmail(props.accountId, challengeId.value, code.value.trim(), props.csrfToken);
    } else {
      result = await deleteAccountEmail(props.accountId, props.email, props.csrfToken);
    }
    if (!gate.isCurrent(ticket, props.accountId)) return;
    code.value = "";
    if (result?.status === "verification_required" && result.challenge_id) {
      challengeId.value = result.challenge_id;
      step.value = "apple-code";
    } else if (result?.status === "authenticated") {
      step.value = "loading";
      await readProfile(ticket);
    } else if (result?.status === "email_verification_required" && result.challenge_id) {
      challengeId.value = result.challenge_id;
      address.value = result.address;
      step.value = "email-code";
    } else if (result?.status === "complete") {
      emit("changed", { action: props.action, address: result.address || props.email });
    } else {
      throw new Error("Apple 返回了无法确认的结果，请重新检查邮箱列表。");
    }
  } catch (caught) {
    if (!gate.isCurrent(ticket, props.accountId)) return;
    handleError(caught);
    // Only explicit invalid-code responses allow retrying the same challenge.
    if (["email-code", "delete"].includes(submittedStep) &&
        !["APPLE_EMAIL_CODE_INVALID", "APPLE_EMAIL_ACCOUNT_AUTH_REQUIRED"].includes(caught?.code)) {
      step.value = "blocked";
    }
  } finally {
    password.value = "";
    if (gate.isCurrent(ticket, props.accountId)) busy.value = false;
  }
}

function restartEmail() {
  if (busy.value) return;
  challengeId.value = "";
  code.value = "";
  error.value = null;
  step.value = "address";
}

function closeDialog() {
  if (busy.value) return;
  gate.invalidate();
  password.value = "";
  code.value = "";
  emit("close");
}

onMounted(prepare);
onBeforeUnmount(() => { gate.deactivate(); password.value = ""; code.value = ""; });
</script>

<style scoped>
.email-lead { margin: 0 0 18px; line-height: 1.7; overflow-wrap: anywhere; }
</style>
