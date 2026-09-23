# Apple 账户电子邮箱管理

本实现依据用户提供的 `apple添加或移除电子邮件.har` 中的请求结构。示例地址和验证标识均为占位内容，不使用抓包中的 Cookie、会话令牌或验证码。

## Apple 接口

| 操作 | 方法与地址 | 请求体 | 成功结果 |
| --- | --- | --- | --- |
| 获取登录服务标识 | `GET https://account.apple.com/bootstrap/portal` | 无 | `serviceKey` |
| 初始化管理会话 | `GET https://appleid.apple.com/account/manage/gs/ws/token` | 无 | HTTP 200，更新会话头 |
| 读取邮箱 | `GET https://appleid.apple.com/account/manage` | 无 | `apiKey`、`appleID`、`primaryEmailAddress`、`alternateEmailAddresses`、`shouldAllowAddAlternateEmail` |
| 发送邮箱验证码 | `POST https://appleid.apple.com/account/manage/email/alternate/add/verification` | `{"address":"new@example.com"}` | HTTP 201，`verificationId`、`address`、`length: 6` |
| 验证并添加 | `PUT https://appleid.apple.com/account/manage/email/alternate/verification` | `{"address":"new@example.com","verificationInfo":{"id":"verification-id","answer":"654321"}}` | HTTP 200，匹配地址的邮箱 ID 和 `vetted: true` |
| 删除备用邮箱 | `DELETE https://appleid.apple.com/account/manage/email/alternate/{id}` | 无 | HTTP 200，`emailAddressRemoved` 包含匹配的邮箱 ID 和地址 |

账户管理的 SRP 登录使用 bootstrap 返回的 `serviceKey`，重定向地址是 `https://account.apple.com`。双重认证使用受信任设备验证码；认证后初始化管理会话，再读取邮箱信息。请求的 `Origin`/`Referer` 指向账户管理站点，`X-Apple-I-Request-Context` 为 `ca`，邮箱写入请求携带读取账户信息获得的 `X-Apple-Api-Key`。每次响应更新 SCNT 和 Cookie。

该会话与已有 iCloud 登录隔离，并随主号的 Apple 会话一起加密保存。首次使用或失效时需要输入当前 Apple 账户密码，可能还需输入设备验证码。添加邮箱的验证码与设备验证码分属两个不同验证流程。

## 本地管理 API

以下路径相对于管理 API 前缀；都需要管理员会话，写入还需 `X-CSRF-Token`。

| 方法与路径 | 请求体 | 结果 |
| --- | --- | --- |
| `GET /accounts/{id}/forwarding/emails` | 无 | `apple_id`、`can_add`、`emails: [{id,address,removable}]` |
| `POST /accounts/{id}/forwarding/email-auth` | `password` | `authenticated` 或 `verification_required` 与 `challenge_id` |
| `POST /accounts/{id}/forwarding/email-auth/verify` | `challenge_id`、`code` | `authenticated` |
| `POST /accounts/{id}/forwarding/emails` | `address` | `email_verification_required`、`challenge_id`、`address` |
| `POST /accounts/{id}/forwarding/emails/verify` | `challenge_id`、`code` | `complete`、`address` |
| `DELETE /accounts/{id}/forwarding/emails` | `address` | `complete`、`address` |

所有结果放在 `data` 中。挑战标识由本服务生成，绑定管理员、主号、Apple 账户和操作类型，默认十分钟有效、最多五次尝试。密码和验证码不持久化，上游邮箱验证标识仅留在服务端内存。服务重启后需要重新开始未完成的验证。

删除前重新读取 iCloud 可转发列表和 Apple 账户邮箱列表，由服务端解析邮箱 ID；仅剩一个可转发邮箱时禁止删除。仅接受 Apple 的备用邮箱条目，主邮箱、登录主标识和无法映射到备用邮箱的 iCloud 别名不能通过此接口删除。不自动更换当前转发目标。写入结果不确定时不重放请求，需要重新查询当前状态。

添加或删除确认成功后，界面重新请求已有转发设置接口，刷新 `forwardToEmails` 与 `selectedForwardTo`。Apple 尚未同步新地址时，可以再次点击“重新获取”。管理电子邮箱不会创建或删除本地隐私邮箱记录。

常见错误：`APPLE_EMAIL_ACCOUNT_AUTH_REQUIRED`（需要账户管理登录）、`APPLE_EMAIL_INVALID`（地址不可用或已添加）、`APPLE_EMAIL_CODE_INVALID`（邮箱验证码无效）、`APPLE_EMAIL_FLOW_EXPIRED`（验证过期）、`APPLE_EMAIL_LAST_ADDRESS`（不能删除最后一个）、`APPLE_EMAIL_NOT_REMOVABLE`（非可删除的备用邮箱）。
