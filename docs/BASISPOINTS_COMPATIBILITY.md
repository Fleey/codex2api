# Excel Basispoints 客户端兼容与回退

账号启用 Excel Basispoints 后，Responses 请求优先使用 BPS 适配器。本补丁处理客户端工具、agent 历史与图片提示值的协议差异，不改变账号的 BPS 开关。

## 工具与历史

- 中继生成的 function 参数为普通 JSON，响应显式携带 `encrypted_function_args: []`。直接工具调用带有明确加密声明时保留该声明。
- 对 `agent_message.content` 中误置于 `encrypted_content` 的明确自然语言明文，转换为 `input_text`，保留作者、接收者和完整文本。真正不透明的内容不做解码或改写。
- 原生 Responses 回放移除不兼容的展示用 `reasoning.content` 和 `status`，保留 `encrypted_content` 与 `summary`。
- 仅对已声明工具且解释唯一的 transport 错误进行恢复：误标 custom 的单个 JSON function 参数对象，或 raw 标记缺少可由声明唯一确定的字段。未声明目标、冲突 envelope、脚本、多个 JSON 值仍拒绝。
- 图片 `detail: "original"` 规范为 `high`；不重编码或修改图片数据。
- 历史中由其他中继（旧网关、excel-codex-bridge、SUB2API 节点）漏给客户端的 `run_officejs` 调用按原样回放，不再套第二层 `run_officejs`，避免模型照抄出嵌套信封。

## 工具搜索与目录大小

- 客户端执行的 `tool_search`（`execution` 为空或 `client`）作为目录工具经 `run_officejs` 中继，客户端收到 `tool_search_call`（无参数增量事件，`output_item.added` 后紧跟 `done`）。`query` 必须非空，`limit` 仅保留正整数。服务端执行的 `tool_search` 仍按不可用的托管工具提示。
- 历史中的 `tool_search_output`（及别名 `tool_search_call_output`）回放为该次 `run_officejs` 的文本结果，列出所加载工具的完整定义；这些工具在本请求内可调用，但不进入提示词开头的目录，缓存前缀不变。`tool_choice: none` 时不注册，`allowed_tools` 同样约束它们。
- 无命名空间的 Codex 自身工具与 `collaboration`、`image_gen`、`web` 命名空间完整列出；其余命名空间（桌面版应用工具、插件、MCP 服务器）的 function 工具只列开头句子与各参数类型。历史中调用与其 schema 顶层不符（缺必填、未声明字段、类型不符）时，在该调用结果后附上完整定义，供模型下一次纠正；判断只依赖历史与目录，同一会话每次回放字节相同。
- schema 中仅供校验器使用的 `title`、`$schema` 不再写入目录。

## 上游错误

- BPS 的 `error` 事件不直接转发：Codex 忽略该事件类型，只会看到未完成的流并重试。网关先记录，等待上游的 `response.failed` 或断流（最多 10 秒），统一以一条 `response.failed` 结束。限流等待、放弃后改写为 `invalid_prompt` 的逻辑同样作用于这条 `response.failed`。
- 错误码默认替换为通用的 `basispoints_upstream_error`。只有仅取决于请求本身、Codex 按码处理的 `context_length_exceeded`（Codex 据此压缩对话）与 `invalid_prompt`（展示而不重试）保留原码，消息换成网关固定文案，不转发上游消息。配额、套餐、过载类错误码与账号相关，保持通用码，让客户端重试时有机会落到健康账号。
- `slow_down` 与 `rate_limit_exceeded` 一样按限流等待。

## 上下文窗口（1M）

BPS 后端实测最多接受约 918k 输入 token（excel-codex-bridge 对 gpt-5.6-sol、gpt-6-sol、gpt-6-luna、gpt-6-astra 实测相同），网关按其 1M 版的规则使用：

| 位置 | 取值 |
| --- | --- |
| Codex 模型清单 `context_window` / `max_context_window` | 918000 |
| Codex 模型清单 `auto_compact_token_limit`（Codex 自己压缩） | 826000（90%） |
| Codex 模型清单 `effective_context_window_percent` | 95 |
| 发给 BPS 的 `context_management.compact_threshold`（后端兜底压缩） | 872000（95%） |

- 清单只对「该 key 可见的 Codex 账号里，凡能服务该模型的都开了 BPS」的模型改写；号池里还有原生账号能服务该模型时保留官方窗口，避免长对话落到原生账号被拒。改写后的清单使用网关自算的 ETag。
- 客户端自带 `context_management` 时原样保留；`CODEX_EXCEL_BPS_COMPACT_THRESHOLD` 仍可覆盖后端阈值，范围 10000–872000。
- 长对话每轮发送的上下文更多，消耗的共享 TPM 额度也更多。已运行的 Codex（含桌面版 app-server）会沿用缓存的旧模型清单，重启后才按新窗口压缩。

## 请求体大小

请求体上限由全局 `CODEX_MAX_REQUEST_BODY_SIZE_MB`（默认 48）控制，按解压后大小计算。带图片的长对话在 Codex 自动压缩前就可能超过 64 MB；BPS 用户遇到 413 时可调大该值，并确保 `CODEX_REQUEST_MEMORY_BUDGET_MB` 不小于它。该上限对所有推理端点生效，调大前评估并发内存。

## 何时使用原生 Responses

以下条件只在尚未向客户端写入响应且请求未取消时允许进入同一已获取账号的原生处理链路：

| 条件 | 日志 reason |
| --- | --- |
| `previous_response_id` | `stored_response` |
| BPS schema 无法表示的 agent/其他不透明上下文 | `agent_context` / `opaque_context` |
| 本地 BPS 请求准备拒绝为不支持 | `unsupported_request` |
| BPS 上游 HTTP 5xx | `upstream_5xx` |
| 精确 HTTP 403 + `basispoints_model_access_changed` | `model_access` |
| 传输错误或空响应 | `transport_error` |

回退响应带有 `X-Codex2api-Upstream-Fallback: basispoints-to-codex`。已发生的 BPS 失败保留为 retry-attempt 用量记录；提前判定不适合 BPS 的请求不伪造一次上游尝试。回退不替换模型、不切换 BPS 设置；后续原生处理继续使用既有调度及错误处理规则。

普通 401/403、上游使用策略拒绝、已输出后的失败及请求取消不会触发此回退。不要将“请求被使用策略拒绝”直接解释为永久封号：该提示本身未给出具体触发规则。

## 验证边界

单元和 handler 集成测试覆盖参数、历史、图片 detail、回退与账号释放。模拟 agent 协议通过不代表所有客户端的真实子代理流程已端到端验证。`tool_search` 的请求与历史形状按 Codex 现有行为（与 Grok 适配器、excel-codex-bridge 一致）模拟，尚未用真实 Codex 经 BPS 账号端到端验证。

本补丁不增加 BPS WebSocket，也不提供上游首字速度保证。HTTP/SSE 请求、文本首 delta 与完整工具参数可执行时间属于不同观测点，应分开测量。
