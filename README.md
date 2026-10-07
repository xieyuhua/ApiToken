# apitoken · 多平台大模型 API 网关

一个用 Go 编写的 **OpenAI 兼容** 大模型网关：把 **DeepSeek**、**商汤日日新 SenseNova**、**腾讯云 WorkBuddy（代码助手）** 以及任意 OpenAI 兼容服务的 API Key 集中管理，对外只暴露一个 `base_url` + 一个网关密钥。

- 统一协议：`POST /v1/chat/completions`（含 SSE 流式）、`GET /v1/models`
- 多渠道：同一模型可绑定多个平台/账号，按优先级 + 权重负载均衡
- 自动故障转移：上游 429/5xx/超时自动切换下一个渠道
- 零依赖运行：单二进制 + 一个 YAML 配置，内置 Web 管理页（无前端构建）
- 可观测：token 用量、成功率、平均延迟、最近 500 条访问日志
- 鉴权：客户端 Key（`Authorization: Bearer`）与管理令牌分离

---

## 1. 快速开始

```bash
# Windows
go build -o apitoken.exe ./cmd/gateway
.\apitoken.exe -config config.yaml

# Linux / macOS
go build -o apitoken ./cmd/gateway
./apitoken -config config.yaml
```

启动后：

| 地址 | 说明 |
| --- | --- |
| http://127.0.0.1:8080/ | Web 控制台：渠道、路由、用量看板（填入管理令牌 `adm-0001`） |
| http://127.0.0.1:8080/logs | 调用日志：筛选、分页、故障转移链路、导出 |
| http://127.0.0.1:8080/v1 | OpenAI 兼容接口（网关密钥 `sk-gateway-0001`） |
| http://127.0.0.1:8080/admin/… | 管理 API |
| http://127.0.0.1:8080/healthz | 健康检查 |

命令行参数：`-config`（配置路径）、`-addr`（覆盖监听地址）、`-version`。

> 渠道/路由的改动会持久化到 `data/gateway.json`，重启后依然生效；配置文件里新增的渠道会自动补进来。

---

## 2. 接入平台（填 Base URL 即可）

网关只做一件事：把请求按 OpenAI 兼容协议转发到上游的 `{base_url}/chat/completions`，并注入 `Authorization: Bearer {api_key}`。
所以接任何平台都只需要 **Base URL + API Key + 模型名** 三样，不需要选“平台类型”。

| 平台 | Base URL |
| --- | --- |
| DeepSeek | `https://api.deepseek.com/v1` |
| 商汤日日新（Token Plan） | `https://token.sensenova.cn/v1` |
| 商汤（大装置 compatible-mode） | `https://api.sensenova.cn/compatible-mode/v2` |
| 腾讯云 WorkBuddy / 代码助手（标准版） | `https://api.copilot.tencent.com/api/v1` |
| 腾讯云 WorkBuddy（专享版） | `https://{enterpriseId}.copilot.qq.com/api/v1` |
| OpenCode Zen | `https://opencode.ai/zen/v1` |
| 阶跃星辰 StepFun（按量） | `https://api.stepfun.com/v1` |
| 阶跃星辰 StepFun（Coding Plan） | `https://api.stepfun.com/step_plan/v1` |
| OpenAI | `https://api.openai.com/v1` |

### 2.1 DeepSeek

| 项 | 值 |
| --- | --- |
| base_url | `https://api.deepseek.com/v1` |
| 鉴权 | `Authorization: Bearer <api_key>` |
| 模型 | `deepseek-chat`、`deepseek-reasoner`（推理模型，思维链在 `reasoning_content`） |
| 申请 Key | https://platform.deepseek.com/api_keys |

### 2.2 商汤 日日新 SenseNova

| 项 | 值 |
| --- | --- |
| base_url（Token Plan / OpenAI 兼容） | `https://token.sensenova.cn/v1` |
| base_url（大装置 compatible-mode） | `https://api.sensenova.cn/compatible-mode/v2` |
| 鉴权 | `Authorization: Bearer <api_key 或 api_token>` |
| 模型 | 以控制台为准，如 `SenseNova-6.8-Flash-Lite`；可在界面点「拉取模型」自动获取 |

### 2.3 腾讯云 WorkBuddy（代码助手）

| 项 | 值 |
| --- | --- |
| base_url（标准版） | `https://api.copilot.tencent.com/api/v1` |
| base_url（专享版） | `https://{enterpriseId}.copilot.qq.com/api/v1` |
| 鉴权 | `Authorization: Bearer <API Key>` |
| 模型 | 由账号套餐决定，建议在界面点「拉取模型」；拉取失败时手动填写或留空（留空=放通全部模型名） |

文档：https://www.workbuddy.cn/apiDocs/index.html

### 2.4 OpenCode Zen（订阅 / 按量）

| 项 | 值 |
| --- | --- |
| base_url | `https://opencode.ai/zen/v1` |
| 鉴权 | `Authorization: Bearer <Zen API Key>`（环境变量 `OPENCODE_API_KEY`） |
| 可走 `/chat/completions` 的模型 | `deepseek-v4-pro`、`deepseek-v4.1-flash`、`kimi-k2.7-code`、`glm-5.3`、`minimax-m3` 等 |
| 文档 | https://open-code.ai/docs/zen |

⚠️ Zen 按模型族分端点：Claude 系走 `/messages`、GPT 系走 `/responses`、Gemini 走 `/models/{id}`，只有 Chat Completions 类模型能直接经本网关转发。若要接 Claude/GPT，在渠道里把「Chat 路径」改成对应路径（`/messages` 或 `/responses`），并自行做协议转换。

### 2.5 阶跃星辰 StepFun

| 项 | 值 |
| --- | --- |
| base_url（按量） | `https://api.stepfun.com/v1` |
| base_url（Coding Plan 订阅） | `https://api.stepfun.com/step_plan/v1` |
| 鉴权 | `Authorization: Bearer <api_key>` |
| 模型 | `step-3.5-flash`、`step-2-16k`、`step-1x-32k` 等，以控制台为准 |
| 文档 | https://platform.stepfun.com/docs |

---

## 3. 添加渠道（Web 界面）

1. 打开 http://127.0.0.1:8080/ ，右上角填入 `security.admin_token`（默认 `adm-0001`）并保存。
2. 「添加渠道」→ 填 **Base URL**（输入框有常用地址提示，填到 `/v1` 这一层，不要带 `/chat/completions`）与 **API Key**。
3. 「支持的模型」填模型名（逗号分隔，留空表示放通全部），保存。
4. 点「**测试连通性**」验证（会真实发一条 `ping` 请求，结果直接显示在弹窗内）；点「**拉取模型**」从上游 `/models` 拉取模型列表，勾选后一键写入「支持的模型」。这两个操作在保存之前就能用，草稿不会落库。
5. 需要多账号时，重复添加多个渠道，用 **优先级**（越小越优先）与 **权重** 控制分流。

命令行等价操作：

```bash
curl -X POST http://127.0.0.1:8080/admin/channels \
  -H "X-Admin-Token: adm-0001" -H "Content-Type: application/json" \
  -d '{"id":"deepseek-1","name":"DeepSeek 官方","base_url":"https://api.deepseek.com/v1","api_key":"sk-xxx","models":["deepseek-chat"],"priority":10,"enabled":true}'
```

---

## 4. 调用方式

### curl

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-gateway-0001" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"你好"}]}'
```

### OpenAI SDK（Python）

```python
from openai import OpenAI

client = OpenAI(api_key="sk-gateway-0001", base_url="http://127.0.0.1:8080/v1")

print(client.chat.completions.create(
    model="deepseek-chat",                       # 换成 SenseNova / WorkBuddy 上的模型名即可
    messages=[{"role": "user", "content": "你好"}],
).choices[0].message.content)

stream = client.chat.completions.create(
    model="deepseek-chat",
    messages=[{"role": "user", "content": "写首诗"}],
    stream=True,
)
for chunk in stream:
    print(chunk.choices[0].delta.content or "", end="", flush=True)
```

### OpenAI SDK（Go）

```go
client := openai.NewClientWithOptions("http://127.0.0.1:8080/v1", openai.WithToken("sk-gateway-0001"))
```

其它客户端（Cline、Chatbox、NextChat、OpenCode 等）只需把 `base_url` 指向 `http://127.0.0.1:8080/v1`，API Key 填网关密钥。

---

## 5. 路由与故障转移

**优先级 1：显式路由（routes）** —— 一个对外模型名绑定有序渠道列表，严格按声明顺序尝试：

```yaml
routes:
  - model: "gpt-4o"
    channels: ["tencent-1", "sensenova-1"]      # 腾讯挂了自动切商汤
    model_map:
      tencent-1: "gpt-4o-0613"                  # 该渠道上的真实模型名
```

**优先级 2：自动路由** —— 未显式配置的模型，按各渠道 `models` / `alias` 字段匹配，再按 `routing.strategy` 排序：

| 策略 | 行为 |
| --- | --- |
| `priority_round_robin`（默认） | 先按 `priority` 分组，组内按 `weight` 加权轮询 |
| `round_robin` | 忽略优先级，整体加权轮询 |
| `failover` | 严格按优先级，从不轮询（适合主备） |
| `random` | 随机 |

**重试规则**：`routing.max_attempts` 限制单请求最多尝试的渠道数；仅当错误码命中 `routing.retry_status`（默认 408/409/425/429/5xx）或发生网络错误时才切换渠道，4xx（如 401 Key 失效）直接返回，避免无谓重试。流式请求在**尚未下发响应头之前**也可以安全切换渠道。

渠道还支持：

- `auth_style`：`bearer`（默认）/ `header`（key 放进自定义头）/ `query`（`?key=`），配合 `auth_header` 使用

- `alias`：`{"对外模型名": "上游模型名"}`，如把腾讯的 `deepseek-v3.2` 暴露成 `deepseek-chat`
- `extra_params`：给该渠道固定注入参数（如 `{"thinking": {"type": "enabled"}}`）
- `extra_headers`：附加请求头（如腾讯云企业 ID 头）
- `timeout_sec`：单独的超时时间

---

## 6. 管理 API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/admin/api-info` | 版本、平台预设、路由策略、鉴权状态 |
| GET | `/admin/config` | 当前配置（密钥脱敏） |
| GET | `/admin/channels` | 渠道列表（`?reveal=1` 返回完整 Key） |
| POST | `/admin/channels` | 新增渠道（自动套用平台预设） |
| GET/PUT/PATCH/DELETE | `/admin/channels/{id}` | 查看 / 全量更新 / 局部更新 / 删除 |
| POST | `/admin/channels/{id}/toggle` | 启用/停用，body `{"enabled":false}` |
| POST | `/admin/channels/{id}/test` | 已保存渠道的连通性测试，body `{"model":"...","prompt":"ping"}` |
| POST | `/admin/channels/test` | **草稿测试**：直接提交渠道配置测试，不落库（添加渠道弹窗使用） |
| POST | `/admin/channels/models` | **草稿拉模型**：提交渠道配置拉取上游 `/models`，不落库 |
| POST | `/admin/channels/{id}/models` | 拉取上游模型，body `{"apply":true}` 直接写回渠道 |
| GET/POST | `/admin/routes` | 路由列表 / 新增或更新路由 |
| DELETE | `/admin/routes/{model}` | 删除路由 |
| GET | `/admin/stats` | 用量、成功率、延迟、各渠道状态 |
| GET | `/admin/logs?limit=80` | 最近访问日志（数组模式） |
| GET | `/admin/logs?page=1` | 日志分页与聚合统计（page、page_size、model、channel、status、stream、keyword） |
| GET | `/admin/logs/facets` | 日志筛选项（模型、渠道、平台） |
| GET | `/admin/logs/{request_id}` | 一次请求的全部尝试链路 |
| GET | `/admin/logs/export?format=csv` | 导出 CSV（format=json 导出 JSON） |
| DELETE | `/admin/logs` | 清空调用日志 |
| GET | `/healthz` `/readyz` | 健康 / 就绪检查 |

---

## 7. 配置项

```yaml
server:
  addr: ":8080"
  request_timeout: 300s     # 非流式整体超时
  stream_timeout: 900s      # 流式整体超时
  max_body_mb: 32
  data_dir: "./data"

security:
  client_keys: ["sk-gateway-0001"]   # 留空 = 不校验（内网使用）
  admin_token: "adm-0001"            # 留空 = 管理接口不校验

routing:
  strategy: "priority_round_robin"
  max_attempts: 3
  force_stream_usage: false         # 开启后自动注入 stream_options 统计流式 token
  retry_status: [408, 409, 425, 429, 500, 502, 503, 504, 529]

upstream:
  default_timeout: 120s
  proxy_url: ""    # 留空=遵循环境变量 HTTP_PROXY/HTTPS_PROXY；填 none=强制直连；也可填代理地址
```

---

## 8. 常见问题

**Q：返回 `404 Route Not Found`？**
先在浏览器里「测试」渠道。若 curl 直连上游同样 404，说明是网络出口/代理问题；把 `upstream.proxy_url` 设为实际代理，或设为 `none` 强制直连。

**Q：上游 401 / Forbidden？**
Key 无效或未生效。DeepSeek、商汤、腾讯 WorkBuddy 的 Key 各自独立，注意不要混用；腾讯还需确认企业版套餐与 API Key 所属企业。

**Q：模型名找不到（404 model_not_found）？**
`GET /v1/models` 会列出网关当前能路由的模型；渠道 `models` 留空表示放通全部模型名（慎用），否则必须显式列出或配置 `alias`。

**Q：如何做主备？**
同一个模型配两个渠道，`priority` 分别设 1 和 2，策略用 `failover`；或用显式 routes 按顺序声明。

**Q：密钥安全？**
- 配置文件与 `data/gateway.json` 中的 Key 不会出现在管理 API 响应里（`?reveal=1` 除外），Web 页面与日志均脱敏。
- 生产环境请修改 `client_keys` 与 `admin_token`，并用 HTTPS 反向代理对外暴露。
- 日志文件与 `data/` 目录不要提交到代码仓库（见 `.gitignore`）。

---

## 9. 项目结构

```
cmd/gateway/           # 入口：配置加载、HTTP 服务、优雅退出
internal/config/       # YAML 配置解析与默认值
internal/model/        # 渠道、路由、统计、日志数据结构
internal/store/        # 渠道路由解析、负载均衡、统计、访问日志、持久化
internal/proxy/        # OpenAI 协议转发、SSE 流式、故障转移、上游探测
internal/admin/        # 管理 REST API
internal/gateway/      # 路由组装与中间件（鉴权/CORS/恢复/请求 ID）
internal/webui/        # 内嵌管理页面（控制台 + logs.html 调用日志页 + 静态资源）
```

## 10. 调用日志页面

访问 http://127.0.0.1:8080/logs ：

- 统计卡：命中条数、成功率、故障转移次数、Token 用量、平均/P95 延迟
- 明细表：时间、模型（含映射后的上游模型）、渠道/平台、流式/普通、状态码、耗时、尝试次数、Token、错误或响应摘要
- 筛选：模型、渠道、成功/失败/具体状态码、流式、关键字（Request ID / 错误 / 内容）
- 详情弹窗：按 Request ID 展示**每一次上游尝试**（渠道、端点、状态、耗时、错误、请求/响应摘要），便于复盘故障转移
- 聚合分析：按模型 / 渠道 / 状态码的次数与平均延迟、高频错误 Top 10
- 导出与清空：`GET /admin/logs/export?format=csv|json`、`DELETE /admin/logs`

日志默认存内存（条数由 `logging.keep_logs` 控制，默认 1000，重启清空）。需要记录请求/响应摘要时打开 `logging.record_payload`（可能含敏感内容，生产环境谨慎开启）。

运行测试（含 mock 上游的故障转移、流式、管理接口用例）：

```bash
go test ./...
```
