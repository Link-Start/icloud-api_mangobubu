const DIRECTORY_STATES = Object.freeze({
  APPLE_ALIAS_NOT_FOUND: Object.freeze({
    label: "Apple 已不存在",
    type: "warning",
    description: "下次同步完整 Apple 目录时，自动移除本地地址及关联数据。",
  }),
  APPLE_ALIAS_INACTIVE: Object.freeze({
    label: "Apple 已停用",
    type: "info",
    description: "本地归档保留；在 iCloud 启用并同步目录后，可手动启用。",
  }),
});

export function appleAliasDirectoryStatus(alias) {
  if (!alias?.address) return null;
  const code = String(alias.lastSyncError || "").trim();
  return Object.hasOwn(DIRECTORY_STATES, code) ? DIRECTORY_STATES[code] : null;
}
