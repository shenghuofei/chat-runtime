# 配置详解（Configuration）

chat-runtime 使用单个 YAML 配置文件描述 Provider、模型、对话、工具、MCP Server、上下文管理与服务端行为。本文档给出完整的配置结构、逐项参数说明、环境变量覆盖规则以及可直接使用的示例。

运行时通过 `--config` 指定配置文件：

```bash
./chat-runtime --config config.yml                 # CLI 交互模式
./chat-runtime serve --config config.yml --port 8080 # Web 服务模式
./chat-runtime --config config.yml --once "..."      # 一次性任务
```

---

## 1. 完整配置文件结构

一份完整配置由以下顶层字段组成：

```yaml
providers:        # LLM 提供商定义（连接凭证、endpoint）
  <name>: { ... }

models:           # 模型定义（引用 provider + 具体模型名 + 采样参数）
  <name>: { ... }

chats:            # 对话定义（引用 model + system 提示词 + 工具/上下文策略）
  <name>: { ... }

mcp_servers:      # MCP Server 定义（工具来源）
  <name>: { ... }

tools:            # 内置工具配置（命令执行白名单、文件系统等）
  ...

context_manager:  # 上下文溢出策略
  ...

server:           # Web 服务配置（端口、鉴权）
  ...
```

各段之间通过**名称引用**串联：`chats` 引用 `models` 的名字，`models` 引用 `providers` 的名字。这种命名引用使得同一个 provider 可被多个 model 复用、同一个 model 可被多个 chat 复用。

---

## 2. providers 配置

`providers` 定义 LLM 提供商的连接信息。key 为自定义名称，供 `models` 引用。

### 2.1 通用参数

| 参数        | 类型   | 必填 | 说明                                                   |
|-------------|--------|------|--------------------------------------------------------|
| `type`      | string | 是   | Provider 类型，见下表支持列表                           |
| `api_key`   | string | 视类型而定 | API 密钥。建议用环境变量注入，见第 9 节                |
| `base_url`  | string | 否   | 自定义 API endpoint，用于代理、私有部署或兼容网关       |
| `timeout`   | string | 否   | 请求超时时间，如 `60s`、`2m`                            |
| `headers`   | map    | 否   | 额外的自定义请求头，适用于代理鉴权、路由标记等场景      |

### 2.2 支持的 Provider 类型

| type       | 提供商          | 说明                                              |
|------------|-----------------|---------------------------------------------------|
| `openai`   | OpenAI          | 也可通过 `base_url` 对接 OpenAI 兼容网关           |
| `claude`   | Anthropic Claude| 支持 Prompt Cache（cache_control）                |
| `deepseek` | DeepSeek        | 性价比高，支持长上下文                             |
| `ollama`   | Ollama          | 本地模型，通常配置 `base_url: http://localhost:11434` |
| `ark`      | 火山引擎 Ark    | 字节跳动火山方舟                                   |
| `gemini`   | Google Gemini   | 需 Google API Key                                 |
| `qwen`     | 阿里通义千问     | 兼容 OpenAI 风格接口                               |

### 2.3 各 Provider 配置示例

```yaml
providers:
  # OpenAI
  openai:
    type: openai
    api_key: ${OPENAI_API_KEY}
    base_url: https://api.openai.com/v1   # 可选，默认官方地址
    timeout: 60s

  # Claude
  claude:
    type: claude
    api_key: ${ANTHROPIC_API_KEY}

  # DeepSeek
  deepseek:
    type: deepseek
    api_key: ${DEEPSEEK_API_KEY}
    base_url: https://api.deepseek.com

  # Ollama（本地，无需 api_key）
  ollama:
    type: ollama
    base_url: http://localhost:11434

  # 火山引擎 Ark
  ark:
    type: ark
    api_key: ${ARK_API_KEY}

  # Google Gemini
  gemini:
    type: gemini
    api_key: ${GEMINI_API_KEY}

  # 通义千问 Qwen
  qwen:
    type: qwen
    api_key: ${DASHSCOPE_API_KEY}
    base_url: https://dashscope.aliyuncs.com/compatible-mode/v1
```

---

## 3. models 配置

`models` 定义具体使用的模型，绑定一个 provider 并指定模型名与采样参数。key 为自定义名称，供 `chats` 引用。

| 参数            | 类型    | 必填 | 说明                                                 |
|-----------------|---------|------|------------------------------------------------------|
| `provider`      | string  | 是   | 引用 `providers` 中的名称                            |
| `model`         | string  | 是   | Provider 侧的实际模型标识，如 `gpt-4o`、`deepseek-chat` |
| `temperature`   | float   | 否   | 采样温度，0~2，越高越发散                             |
| `max_tokens`    | int     | 否   | 单次响应的最大生成 token 数                          |
| `top_p`         | float   | 否   | 核采样参数                                           |
| `reasoner`      | bool    | 否   | 显式声明为推理模型（如 `deepseek-reasoner`、`o1` 等）。推理模型通常不支持 `temperature`/`top_p`；若不设置，某些 Provider 会根据模型名做启发式判断 |

```yaml
models:
  default:
    provider: deepseek
    model: deepseek-chat
    temperature: 0.7
    max_tokens: 4096

  gpt4o:
    provider: openai
    model: gpt-4o
    temperature: 0.3

  local-llama:
    provider: ollama
    model: llama3.1
```

---

## 4. chats 配置

`chats` 是运行时真正加载的对话配置，绑定一个 model，并定义 system 提示词、可用工具与上下文策略。默认加载名为 `default` 的对话，可通过命令行参数切换。

| 参数              | 类型          | 必填 | 说明                                                     |
|-------------------|---------------|------|----------------------------------------------------------|
| `model`           | string        | 是   | 引用 `models` 中的名称                                   |
| `system`          | string        | 否   | 系统提示词，支持 `@file:` 引用与 Go 模板变量（见 4.1/4.2）|
| `default`         | bool          | 否   | 是否为默认对话（未指定 `--chat` 时加载）。多个 `true` 时行为不确定，建议只设一个 |
| `max_iterations`  | int           | 否   | Tool Calling 循环的最大迭代次数，防止无限循环（0 表示不限）|
| `tools`           | list<string>  | 否   | 该对话可用的工具/工具组，缺省表示全部可用                 |
| `mcp_servers`     | list<string>  | 否   | 该对话启用的 MCP Server 名称，缺省表示全部启用            |
| `context_manager` | object        | 否   | 覆盖全局 `context_manager`，见第 7 节                    |

```yaml
chats:
  default:
    model: default
    system: "你是一个有用的助手。"

  devops:
    model: gpt4o
    system: "@file:./prompts/devops.md"
    mcp_servers:
      - k8s-eye
    tools:
      - command
      - filesystem
```

### 4.1 system 支持 @file: 引用

`system` 字段的值以 `@file:` 前缀开头时，运行时会读取指定文件的内容作为 system 提示词。路径相对于配置文件所在目录（或使用绝对路径）。

```yaml
chats:
  default:
    model: default
    system: "@file:./prompts/system.md"
```

这便于将较长的提示词维护在独立文件中，与配置解耦，并可复用于版本管理。

### 4.2 system 支持 Go 模板变量

`system` 的内容（无论内联还是通过 `@file:` 载入）会经过 Go `text/template` 渲染，支持以下内置变量：

| 变量        | 含义                         | 示例值                    |
|-------------|------------------------------|---------------------------|
| `{{.Cwd}}`  | 当前工作目录                 | `/Users/alice/project`    |
| `{{.Date}}` | 当前日期/时间                | `2026-09-23`              |
| `{{.User}}` | 当前用户名                   | `alice`                   |
| `{{.Home}}` | 当前用户主目录               | `/Users/alice`            |

示例：

```yaml
chats:
  default:
    model: default
    system: |
      你是一个运行在用户本地环境的助手。
      当前用户：{{.User}}
      主目录：{{.Home}}
      工作目录：{{.Cwd}}
      当前日期：{{.Date}}
      在给出文件路径时，请优先使用相对于工作目录的相对路径。
```

配合 `@file:` 使用时，文件内容同样会被模板渲染：

```yaml
chats:
  default:
    model: default
    system: "@file:./prompts/system.md"   # 文件中可直接写 {{.Cwd}} 等变量
```

---

## 5. mcp_servers 配置

`mcp_servers` 定义 MCP（Model Context Protocol）工具来源。key 为自定义名称，其工具会以 `<server_name>__<tool_name>` 形式暴露给模型。详细集成说明见 [mcp.md](mcp.md)。

| 参数                  | 类型                | 必填 | 说明                                                             |
|-----------------------|---------------------|------|------------------------------------------------------------------|
| `transport`           | string              | 是   | 传输方式：`stdio` / `sse` / `streamable-http`                    |
| `command`             | string              | stdio 必填 | stdio 模式下要启动的可执行文件                              |
| `args`                | list<string>        | 否   | stdio 模式下的命令行参数                                          |
| `env`                 | map<string,string>  | 否   | stdio 模式下注入子进程的环境变量                                  |
| `url`                 | string              | sse/http 必填 | sse / streamable-http 模式的服务地址                      |
| `headers`             | map<string,string>  | 否   | sse / streamable-http 模式的自定义请求头（如鉴权）               |
| `auto_approve`        | bool 或 list<string>| 否   | 审批策略：`true` 全部自动 / `[tool...]` 指定 / `false` 全部需审批 |
| `include`             | list<string>        | 否   | 工具白名单，仅暴露列出的工具                                      |
| `exclude`             | list<string>        | 否   | 工具黑名单，屏蔽列出的工具                                        |
| `no_concurrent`       | bool                | 否   | 该 Server 的工具调用整体串行执行                                  |
| `no_concurrent_tools` | list<string>        | 否   | 指定这些工具之间串行执行                                          |

```yaml
mcp_servers:
  # stdio：本地子进程
  k8s-eye:
    transport: stdio
    command: ./k8s-eye
    args: ["--verbose"]
    env:
      KUBECONFIG: ${HOME}/.kube/config
    auto_approve: true

  # SSE：远程服务
  web-search:
    transport: sse
    url: https://your-mcp-host/sse
    headers:
      Authorization: Bearer ${MCP_TOKEN}
    auto_approve:
      - search

  # Streamable HTTP
  data-api:
    transport: streamable-http
    url: https://your-mcp-host/mcp
    include:
      - query_metric
      - list_dashboards
```

---

## 6. tools 配置

`tools` 配置内置工具的行为，最重要的是命令执行白名单与文件系统限制。

| 参数                  | 类型          | 说明                                                     |
|-----------------------|---------------|----------------------------------------------------------|
| `command.enabled`     | bool          | 是否启用命令执行工具                                     |
| `command.whitelist`   | list<string>  | 命令白名单，仅允许列出的命令执行；为空且启用白名单时默认拒绝 |
| `command.auto_approve`| bool 或 list  | 命令执行的审批策略，规则同 `auto_approve`                |
| `command.timeout`     | string        | 单条命令的执行超时                                       |
| `filesystem.enabled`  | bool          | 是否启用文件操作工具                                     |
| `filesystem.root`     | string        | 文件操作限定的根目录，越界访问被拒绝                     |
| `filesystem.readonly` | bool          | 是否只读，禁止写入/删除                                   |

```yaml
tools:
  command:
    enabled: true
    whitelist:
      - ls
      - cat
      - grep
      - kubectl
    auto_approve:
      - ls
      - cat
      - grep       # 只读命令自动批准
    timeout: 30s

  filesystem:
    enabled: true
    root: ./workspace
    readonly: false
```

> 安全建议：命令执行工具默认应遵循最小权限——显式列出允许的命令，只对只读命令 `auto_approve`，其余需人工审批。参见 [architecture.md](architecture.md) 第 6 节安全设计。

---

## 7. context_manager 配置

`context_manager` 定义上下文溢出策略。可放在顶层作为全局默认，也可在某个 `chats.<name>.context_manager` 中覆盖。三种模式的原理详见 [architecture.md](architecture.md) 第 3 节。

| 参数             | 类型   | 适用模式            | 说明                                                     |
|------------------|--------|---------------------|----------------------------------------------------------|
| `mode`           | string | -                   | 溢出策略：`compress` / `truncate` / `window`             |
| `max_rounds`     | int    | truncate            | 保留的最大对话轮次数                                     |
| `max_tokens`     | int    | window / compress   | token 预算上限（window 的窗口大小 / compress 的触发阈值）|
| `compress_ratio` | float  | compress            | 压缩目标比例，如 `0.5` 表示压缩到约一半                   |

三种模式配置示例：

```yaml
# 模式一：compress —— 摘要压缩早期历史
context_manager:
  mode: compress
  max_tokens: 32000       # 达到该阈值触发压缩
  compress_ratio: 0.5     # 压缩后目标降到约一半

---
# 模式二：truncate —— 保留最近 N 轮
context_manager:
  mode: truncate
  max_rounds: 20

---
# 模式三：window —— token 滑动窗口
context_manager:
  mode: window
  max_tokens: 16000
```

按对话覆盖全局：

```yaml
context_manager:          # 全局默认
  mode: truncate
  max_rounds: 20

chats:
  long-task:
    model: default
    context_manager:      # 该对话单独使用 compress
      mode: compress
      max_tokens: 64000
      compress_ratio: 0.4
```

---

## 8. server 配置

`server` 配置 Web 服务模式（`serve` 子命令）的监听与鉴权。命令行参数（如 `--port`）优先级高于配置文件。

| 参数                   | 类型     | 默认      | 说明                                                               |
|------------------------|----------|-----------|--------------------------------------------------------------------|
| `host`                 | string   | `0.0.0.0` | 监听地址                                                           |
| `port`                 | int      | `8080`    | 监听端口                                                           |
| `basic_auth.enabled`   | bool     | `false`   | 是否启用 HTTP Basic Auth                                           |
| `basic_auth.username`  | string   | -         | Basic Auth 用户名                                                  |
| `basic_auth.password`  | string   | -         | Basic Auth 密码，建议用环境变量注入                                 |
| `max_sessions`         | int      | `100`     | 最大并发会话数，防止连接数无上限导致 DoS。`-1` 表示不限制（公网环境慎用）|
| `approval_timeout`     | duration | `5m`      | Web 审批等待超时，超时后自动拒绝                                   |
| `shutdown_timeout`     | duration | `15s`     | 优雅关闭的最大等待时间，超时后强制停止                             |
| `read_header_timeout`  | duration | `10s`     | HTTP 读取请求头的超时，防范慢速连接攻击                            |
| `ping_interval`        | duration | `30s`     | WebSocket 心跳发送间隔                                             |
| `pong_wait`            | duration | `45s`     | 等待 pong 响应的超时，应大于 `ping_interval`                       |
| `write_wait`           | duration | `10s`     | WebSocket 写操作超时                                               |

```yaml
server:
  host: 0.0.0.0
  port: 8080
  basic_auth:
    enabled: true
    username: admin
    password: ${WEB_PASSWORD}
  max_sessions: 100          # 可选，默认 100
  approval_timeout: 5m       # 可选，默认 5m
  shutdown_timeout: 15s      # 可选，默认 15s
```

> 安全建议：Web 模式会让远程用户触发工具执行，务必在暴露到公网前启用 `basic_auth` 或置于反向代理/鉴权网关之后。

---

## 9. 环境变量覆盖

为避免将密钥硬编码进配置文件，chat-runtime 支持在 YAML 中使用 `${VAR}` 语法进行环境变量插值。加载配置时，`${VAR}` 会被替换为对应环境变量的值。

```yaml
providers:
  openai:
    type: openai
    api_key: ${OPENAI_API_KEY}     # 从环境变量读取

server:
  basic_auth:
    password: ${WEB_PASSWORD}
```

推荐实践：

- 所有密钥（`api_key`、`password`、`token`、`KUBECONFIG` 等）均通过环境变量注入。
- 配置文件本身可安全入库（不含明文密钥）。
- 在 CI/CD 或容器环境中通过 Secret / 环境变量下发真实值。

> 说明：`${VAR}` 未定义时会被替换为空字符串，请确保部署环境已正确设置所需变量。

---

## 10. 完整配置示例

一份涵盖多数字段的示例（可作为起点裁剪）：

```yaml
providers:
  deepseek:
    type: deepseek
    api_key: ${DEEPSEEK_API_KEY}
  openai:
    type: openai
    api_key: ${OPENAI_API_KEY}

models:
  default:
    provider: deepseek
    model: deepseek-chat
    temperature: 0.7
    max_tokens: 4096
  smart:
    provider: openai
    model: gpt-4o
    temperature: 0.3

chats:
  default:
    model: default
    system: |
      你是运行在 {{.User}} 本地环境的助手。
      工作目录：{{.Cwd}}，当前日期：{{.Date}}。
    context_manager:
      mode: truncate
      max_rounds: 20
  devops:
    model: smart
    system: "@file:./prompts/devops.md"
    mcp_servers:
      - k8s-eye
    tools:
      - command

mcp_servers:
  k8s-eye:
    transport: stdio
    command: ./k8s-eye
    env:
      KUBECONFIG: ${HOME}/.kube/config
    auto_approve:
      - get_pods
      - describe_pod

tools:
  command:
    enabled: true
    whitelist: [ls, cat, grep, kubectl]
    auto_approve: [ls, cat, grep]
    timeout: 30s
  filesystem:
    enabled: true
    root: ./workspace

context_manager:
  mode: truncate
  max_rounds: 20

server:
  host: 0.0.0.0
  port: 8080
  basic_auth:
    enabled: true
    username: admin
    password: ${WEB_PASSWORD}
```

更多可直接运行的示例见 [`examples/`](../examples/) 目录：

- `examples/minimal.yml` — 最小可用配置
- `examples/multi-provider.yml` — 多 Provider 混合模型
- `examples/k8s-ops.yml` — 结合 MCP k8s-eye 的运维配置
- `examples/full.yml` — 含全部选项与注释的完整配置
