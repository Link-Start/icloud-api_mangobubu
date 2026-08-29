export const AUTH_REQUIRED = "AUTH_REQUIRED";
export const SESSION_EXPIRED = "SESSION_EXPIRED";

const EXPLICIT_LOGIN_NOTICES = new Set([
  "credentials_rotated",
  "credentials_changed",
  "rotation_result_invalid",
  "rotation_status_unknown",
]);

export function loginNoticeRequiresExplicitLogin(notice = "") {
  return EXPLICIT_LOGIN_NOTICES.has(String(notice || ""));
}

export function buildLoginRedirect(errorCode, redirect) {
  const query = { redirect };
  if (errorCode === SESSION_EXPIRED) {
    query.notice = "session_expired";
  } else if (errorCode && errorCode !== AUTH_REQUIRED) {
    query.notice = "session_error";
  }
  return { name: "login", query };
}

export function loginNoticeMessage({
  notice = "",
  sessionCheckFailed = false,
  dismissed = false,
} = {}) {
  if (dismissed) return "";
  if (notice === "session_expired") {
    return "登录会话已过期，请重新登录。";
  }
  if (notice === "password_changed") {
    return "管理员密码已更新，请使用新密码重新登录。";
  }
  if (notice === "credentials_rotated") {
    return "全部取码令牌与凭据已轮换，请重新登录。";
  }
  if (notice === "credentials_changed") {
    return "登录凭据已在其他请求中变化，请重新登录并确认轮换结果。";
  }
  if (notice === "rotation_result_invalid") {
    return "令牌轮换已提交，但汇总校验失败；请重新登录后检查操作记录。";
  }
  if (notice === "rotation_status_unknown") {
    return "令牌轮换结果暂时无法确认；请重新登录后检查操作记录。";
  }
  if (notice === "session_error" || sessionCheckFailed) {
    return "未能确认现有会话，请重新登录。";
  }
  return "";
}

export function loginNoticeType(notice = "") {
  return notice === "password_changed" || notice === "credentials_rotated"
    ? "success"
    : "warning";
}
