# 管理员远程开通学生账号并发放 API Key 流程

面向外部业务系统封装调用，覆盖「建套餐 → 建用户 → 绑套餐 → 返回 API Key」的完整顺序。

## 1. 通用约定

- Base URL：`https://<你的 new-api 域名>`
- 请求头：`Authorization: Bearer <PAT>`、`Content-Type: application/json`
- 响应信封：
  - 成功：`{"success":true,"message":"","data":{...}}`
  - 失败：`{"success":false,"message":"...","code":"..."}`（HTTP 可能仍为 200，务必以 `success` 判定）
- 管理接口要求调用者角色 ≥ admin。使用 PAT 时按作用域校验（fail-closed），本流程需要：
  - `user:read`：查询用户
  - `user:write`：建用户、发 key、调整额度
  - `billing:write`：建套餐、绑套餐

## 2. 远程凭证（PAT）

平台没有硬编码的系统管理员 key，远程凭证为个人访问令牌（PAT，前缀 `nap_`）。

- 创建（**需浏览器登录 + 二次验证**，不能纯脚本自助）：

  `POST /api/user/access_tokens`

  ```json
  { "name": "biz-system", "scopes": ["user:read", "user:write", "billing:write"], "expires_at": 0 }
  ```

  返回 `data.token` = `nap_...`，只返回一次，保存到业务系统。

- 使用：所有请求携带 `Authorization: Bearer nap_...`。
- 撤销：`DELETE /api/user/access_tokens/:id`。

## 3. 阶段 A：一次性准备

| 序 | 动作 | 接口 | 说明 |
| --- | --- | --- | --- |
| A1 | 确认支付合规 | `POST /api/option/payment_compliance` | body `{"confirmed":true}`；**仅浏览器会话**，root 操作一次。未确认时建套餐/绑套餐返回 `MsgPaymentComplianceRequired` |
| A2 | 创建 PAT | `POST /api/user/access_tokens` | 见第 2 节，作用域含 `user:read,user:write,billing:write` |
| A3 | 创建套餐 | `POST /api/subscription/admin/plans` | 记录返回的 `data.id`，后续复用 |

### A3 创建套餐

`POST /api/subscription/admin/plans`（scope `billing:write`）

```json
{
  "plan": {
    "title": "每日100额度",
    "subtitle": "",
    "price_amount": 0,
    "currency": "USD",
    "duration_unit": "month",
    "duration_value": 1,
    "custom_seconds": 0,
    "total_amount": 100,
    "quota_reset_period": "daily",
    "quota_reset_custom_seconds": 0,
    "upgrade_group": "",
    "downgrade_group": "",
    "max_purchase_per_user": 0,
    "enabled": true,
    "allow_balance_pay": true,
    "allow_wallet_overflow": true,
    "sort_order": 0
  }
}
```

字段说明：

| 字段 | 说明 |
| --- | --- |
| `total_amount` | 每周期总额度（quota 单位，0 = 无限） |
| `quota_reset_period` | `never` / `daily` / `weekly` / `monthly` / `custom`，每日额度用 `daily` |
| `quota_reset_custom_seconds` | `custom` 时自定义周期秒数 |
| `duration_unit` / `duration_value` | 订阅有效期：`hour` / `day` / `month` / `year` / `custom` |
| `max_purchase_per_user` | 每人最多绑定次数，0 = 不限 |
| `allow_wallet_overflow` | 套餐额度用尽后是否回退钱包余额 |

响应：`data` 为套餐对象，含自增 `id`。

## 4. 阶段 B：每个学生循环

有两种方式，二选一：

- **方式一（推荐，一站式）**：直接把业务系统的唯一 id 作为 `external_id`，一次调用完成建号 + 绑套餐 + 发 key（见 4.1）。
- **方式二（三步）**：B1 建号 → B2 取 user_id → B3 绑套餐发 key → B4 交付。

### 4.1 一站式：按业务唯一 id 建号 + 绑套餐 + 发 key

`POST /api/user/provision`（scope `user:write`）

```json
{
  "external_id": "biz-1002",
  "password": "student-password-1",
  "display_name": "张三",
  "plan_id": 5,
  "token_name": "发放"
}
```

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `external_id` | 必填 | 业务系统的唯一 id，存为 New API 的 `username`（≤64 字符） |
| `password` | 空则随机 24 位 | 该账号登录密码，8–128 位 |
| `display_name` | 同 `external_id` | 显示名，≤64 字符 |
| `plan_id` | 0 | > 0 时先绑该套餐 |
| `token_name` / `unlimited_quota` / `expired_time` / `remain_quota` / `group` | 同 4.3 | API Key 参数 |

行为：`external_id` 对应的用户**不存在则创建**（普通用户），**已存在则复用**（不会重复建号），随后可选绑套餐并新建 API Key。

响应：

```json
{
  "success": true,
  "message": "",
  "data": {
    "user_id": 4,
    "username": "biz-1002",
    "token_id": 2,
    "key": "sk-xxxxxxxx",
    "created": true,
    "plan_id": 5,
    "plan_title": "每日100额度",
    "plan_message": ""
  }
}
```

- `created`：本次是否新建了用户（`true` 新建，`false` 复用已有）。
- `key` 为明文 `sk-...`，**只返回这一次**，直接交付学生。

### 方式二：分步调用

严格顺序：B1 → B2 → B3 → B4。

| 序 | 动作 | 接口 |
| --- | --- | --- |
| B1 | 建号 | `POST /api/user/` |
| B2 | 取 user_id | `GET /api/user/search?keyword=<username>&p=1&page_size=10` |
| B3 | 绑套餐 + 发 key | `POST /api/user/:id/provision` |
| B4 | 交付 key 并落库映射 | 读 `data.key` |

### B1 创建用户

`POST /api/user/`（scope `user:write`）

```json
{ "username": "20240001", "password": "student-password-1", "display_name": "张三", "role": 1 }
```

- `role` 必须低于操作者角色；普通用户填 `1`，无需二次验证。
- 响应**只返回 `{"success":true,"message":""}`，不含 user_id**。

### B2 查询 user_id

`GET /api/user/search?keyword=20240001&p=1&page_size=10`（scope `user:read`）

```json
{ "success": true, "data": { "page": 1, "page_size": 10, "total": 1, "items": [ { "id": 123, "username": "20240001", "role": 1 } ] } }
```

取 `data.items[0].id`。

### B3 绑套餐并发放 API Key

`POST /api/user/:id/provision`（scope `user:write`，`:id` = B2 得到的 user_id）

```json
{ "plan_id": 5, "token_name": "发放", "unlimited_quota": true, "expired_time": -1, "remain_quota": 0, "group": "" }
```

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `plan_id` | 0 | > 0 时先绑该套餐（每次调用新建一条订阅，受 `max_purchase_per_user` 限制） |
| `token_name` | `<用户名> API` | Key 名称，最长 50 |
| `unlimited_quota` | true | true 时从用户额度/套餐扣；false 时使用 `remain_quota` |
| `expired_time` | -1 | 秒级时间戳，-1 表示永久 |
| `remain_quota` | 0 | 仅 `unlimited_quota=false` 时生效 |
| `group` | "" | 指定分组；不支持 `"auto"` |

响应：

```json
{
  "success": true,
  "message": "",
  "data": {
    "user_id": 123,
    "username": "20240001",
    "token_id": 9,
    "key": "sk-xxxxxxxx",
    "plan_id": 5,
    "plan_title": "每日100额度",
    "plan_message": ""
  }
}
```

- `data.key` 是明文 `sk-...`，**只返回这一次**，直接交付学生。
- 权限：root 可操作任意用户；普通 admin 只能操作角色更低的用户。
- 行为：可选绑套餐 → 为该用户新建 API Key；操作写入管理审计（不记录 key）。

### B4 交付与落库

- 将 `data.key`（`sk-...`）发给学生。
- 在业务库存 `{业务编号 → user_id, token_id}` 映射，便于后续查询/去重。

## 5. 可选：只绑套餐（拆分调用）

`POST /api/subscription/admin/users/:id/subscriptions`（scope `billing:write`）

```json
{ "plan_id": 5 }
```

或 `POST /api/subscription/admin/bind`：

```json
{ "user_id": 123, "plan_id": 5 }
```

响应 `{"success":true,"data":null}` 或 `{"success":true,"data":{"message":"用户分组将升级到 xxx"}}`。

## 6. 可选：调整钱包额度

`POST /api/user/manage`（scope `user:write`）

```json
{ "id": 123, "action": "add_quota", "mode": "override", "value": 500000 }
```

`mode`：`add`（增加）/ `subtract`（减少）/ `override`（设为精确值）。

## 7. 封装流程（伪代码）

```text
# 阶段 A（一次性）
POST /api/option/payment_compliance   {confirmed:true}          # 浏览器会话
POST /api/user/access_tokens          -> nap_...（保存）          # 浏览器会话 + 二次验证
POST /api/subscription/admin/plans    -> plan_id

# 阶段 B（每个学生）—— 方式一：一站式
POST /api/user/provision  {external_id, plan_id}  -> data.key (sk-...)   # 建号(或用已有)+绑套餐+发key
交付 data.key 给学生

# 阶段 B（每个学生）—— 方式二：三步
POST /api/user/                       # 建号（username = 业务编号）
GET  /api/user/search?keyword=...     # 取 user_id，存映射
POST /api/user/{user_id}/provision    {plan_id}  -> data.key (sk-...)
交付 data.key 给学生
```

## 8. 判定与注意事项

- 统一判定 `success` 字段，失败读 `message`；HTTP 状态可能仍为 200。
- 幂等：
  - 方式一：同 `external_id` 重复调用会复用已有账号（`created:false`），不会重复建号；但每次都会新建一个 key（和一条订阅，若传了 `plan_id`）。
  - 方式二：B1 用户名重复返回失败（`MsgUserExists`），视为已建，直接进 B2；B3 每次新建一条订阅和一个新 key。
- `data.key` 平台侧不可再次取回；丢失后可由学生本人登录在 `POST /api/token/:id/key` 获取，或再次调用发放新 key。
- 使用发放的 key：`Authorization: Bearer sk-...` 调用 `POST /v1/chat/completions`（例如模型 `deepseek-flash`），额度从该学生的套餐按 `quota_reset_period` 扣除。
- 套餐创建/绑定接口均受「支付合规」开关约束，需 root 先执行 A1。
