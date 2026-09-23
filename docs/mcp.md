# MCP 集成指南

本文档介绍 chat-runtime 如何集成 MCP（Model Context Protocol）工具，包括三种传输方式、工具发现与命名、审批策略、并发控制、工具过滤，以及自定义 MCP Server 的开发指引与常见问题。

---

## 1. MCP 协议简介

MCP（Model Context Protocol，模型上下文协议）是一套开放协议，用于标准化 LLM 应用与外部"工具/数据源"之间的交互。它把"能力提供方"抽象为 **MCP Server**，把"能力消费方"抽象为 **MCP Client**。

- **MCP Server**：暴露一组工具（tools）、资源（resources）、提示（prompts）。例如一个 Kubernetes 运维 Server 可能提供 `get_pods`、`describe_pod`、`get_logs` 等工具。
- **MCP Client**：发现 Server 暴露的能力，并按需调用。chat-runtime 内置了完整的 MCP Client 实现。

采用 MCP 的价值在于：**工具与运行时解耦**。任何符合 MCP 规范的 Server 都能即插即用地接入 chat-runtime，无需修改主程序或重新编译。

在 chat-runtime 中，MCP Server 通过配置文件的 `mcp_servers` 段声明，其提供的工具会自动被发现并注册到当前对话的工具集中，供 LLM 在 Tool Calling 循环中调用。

---

## 2. 三种传输方式

chat-runtime 支持三种 MCP 传输方式，通过 `mcp_servers.<name>.transport` 指定。

### 2.1 stdio

通过标准输入/输出与本地子进程通信。chat-runtime 启动指定的可执行文件，并用其 stdin/stdout 交换 MCP 消息。

- **配置**：

```yaml
mcp_servers:
  k8s-eye:
    transport: stdio
    command: ./k8s-eye        # 要启动的可执行文件
    args: ["--verbose"]        # 可选：命令行参数
    env:                       # 可选：注入子进程的环境变量
      KUBECONFIG: ${HOME}/.kube/config
```

- **使用场景**：
  - MCP Server 与 chat-runtime 部署在同一台机器上。
  - Server 是本地 CLI 工具/二进制。
  - 需要通过环境变量传递本地凭证（如 KUBECONFIG）。
- **特点**：无需网络端口，进程生命周期由 chat-runtime 托管，随会话启动/关闭。

### 2.2 sse

通过 Server-Sent Events（SSE）与远程 HTTP 服务通信，Server → Client 方向用 SSE 流式推送。

- **配置**：

```yaml
mcp_servers:
  web-search:
    transport: sse
    url: https://your-mcp-host/sse
    headers:                   # 可选：自定义请求头（鉴权等）
      Authorization: Bearer ${MCP_TOKEN}
```

- **使用场景**：
  - MCP Server 独立部署为远程服务，被多个客户端共享。
  - 需要长连接、服务端主动推送。
- **特点**：跨网络访问，通过 `headers` 携带鉴权信息。

### 2.3 streamable-http

MCP 的可流式 HTTP 传输，基于标准 HTTP 请求/响应并支持流式，是较新的推荐远程传输方式。

- **配置**：

```yaml
mcp_servers:
  data-api:
    transport: streamable-http
    url: https://your-mcp-host/mcp
    headers:
      Authorization: Bearer ${MCP_TOKEN}
```

- **使用场景**：
  - 远程部署，且希望使用比 SSE 更现代、更易经过网关/负载均衡的 HTTP 传输。
  - 与标准 HTTP 基础设施（反向代理、鉴权网关、可观测性）集成。
- **特点**：基于常规 HTTP，防火墙/代理友好，兼具流式能力。

### 传输方式选择速查

| 传输方式          | 通信对象     | 典型场景                     | 鉴权方式          |
|-------------------|--------------|------------------------------|-------------------|
| `stdio`           | 本地子进程   | 本地 CLI/二进制、本地凭证     | env 环境变量      |
| `sse`             | 远程 HTTP    | 远程共享服务、长连接推送      | headers 请求头    |
| `streamable-http` | 远程 HTTP    | 远程服务、网关友好、现代传输  | headers 请求头    |

---

## 3. 工具发现与命名

chat-runtime 在连接 MCP Server 后，会自动向其发起工具发现（list tools），获取该 Server 暴露的所有工具及其 JSON Schema，并注册到当前对话的工具集中。

### 3.1 命名前缀

为避免不同 Server 之间的工具重名冲突，注册时会加上 **Server 名前缀**，最终工具名形如：

```
<server_name>__<tool_name>
```

例如 `k8s-eye` Server 暴露的 `get_pods` 工具，注册后的名称为：

```
k8s-eye__get_pods
```

这带来两个好处：

1. **消除歧义**：不同 Server 即使有同名工具也不会冲突（如 `k8s-eye__get_logs` 与 `docker__get_logs`）。
2. **可读性**：模型与用户都能从工具名直接判断其来源 Server。

### 3.2 include/exclude 过滤后的命名

工具过滤（见第 5 节）针对**原始工具名**（不含前缀）进行匹配；通过过滤后再统一加前缀注册。

---

## 4. auto_approve 配置

MCP 工具在执行前会经过审批（详见 [architecture.md](architecture.md) 第 6 节）。通过 `auto_approve` 声明每个 Server 的审批策略，支持三种粒度。

### 4.1 全部自动批准

`auto_approve: true`：该 Server 下**所有工具**均自动执行，无需人工确认。

```yaml
mcp_servers:
  k8s-eye:
    transport: stdio
    command: ./k8s-eye
    auto_approve: true
```

> 仅建议用于**可信且低风险/只读**的 Server。任何可能造成变更或破坏的工具都不应无条件放行。

### 4.2 指定工具自动批准

`auto_approve: [tool_a, tool_b]`：**仅列表内**的工具自动执行，其余工具仍需人工审批。列表中使用**原始工具名（不含前缀）**。

```yaml
mcp_servers:
  k8s-eye:
    transport: stdio
    command: ./k8s-eye
    auto_approve:
      - get_pods       # 只读，自动批准
      - describe_pod   # 只读，自动批准
      # delete_pod 未列出 → 仍需人工审批
```

这是最推荐的策略：只读操作自动放行，写/删操作人工把关。

### 4.3 全部需审批

`auto_approve: false` 或省略该字段：所有工具都需人工确认。这是最安全的默认策略。

```yaml
mcp_servers:
  dangerous-ops:
    transport: stdio
    command: ./ops-server
    auto_approve: false   # 每个工具调用都要审批
```

---

## 5. 并发控制

在一轮 Tool Calling 中，LLM 可能一次请求多个工具调用。默认情况下这些调用可并发执行以提升效率，但某些工具/Server 不适合并发（如共享状态、非线程安全、有速率限制）。chat-runtime 提供两级串行化控制。

### 5.1 NoConcurrent（Server 级串行）

`no_concurrent: true`：该 Server 的**所有工具调用整体串行执行**，同一时刻只允许一个调用进行中。

```yaml
mcp_servers:
  stateful-server:
    transport: stdio
    command: ./stateful-server
    no_concurrent: true    # 该 Server 的调用全部串行
```

适用于：Server 内部维护共享状态、或整体不支持并发的场景。

### 5.2 NoConcurrentTools（工具级串行）

`no_concurrent_tools: [...]`：仅让**指定的这些工具之间**串行执行，其余工具仍可并发。

```yaml
mcp_servers:
  mixed-server:
    transport: stdio
    command: ./mixed-server
    no_concurrent_tools:
      - write_config     # 这些工具彼此互斥、串行
      - apply_changes
    # 其余只读工具仍可并发
```

适用于：Server 内大部分工具可并发，仅少数写操作需要互斥。

### 5.3 组合建议

- 只读为主的 Server：不设并发限制，充分利用并发。
- 有少量写操作：用 `no_concurrent_tools` 精确串行化写工具。
- 整体非并发安全：用 `no_concurrent: true` 全量串行。

---

## 6. include / exclude 工具过滤

MCP Server 可能暴露大量工具，但当前对话未必都需要。通过 `include` / `exclude` 精简暴露给模型的工具集，可减少提示词体积、降低误用风险、提升缓存稳定性。匹配基于**原始工具名（不含前缀）**。

### 6.1 include（白名单）

只暴露列出的工具，其余全部屏蔽：

```yaml
mcp_servers:
  data-api:
    transport: streamable-http
    url: https://your-mcp-host/mcp
    include:
      - query_metric
      - list_dashboards
    # 该 Server 的其它工具不会注册
```

### 6.2 exclude（黑名单）

暴露除列出之外的所有工具：

```yaml
mcp_servers:
  k8s-eye:
    transport: stdio
    command: ./k8s-eye
    exclude:
      - delete_namespace   # 屏蔽高危工具
      - drain_node
```

### 6.3 优先级

当同时配置 `include` 与 `exclude` 时，建议以 `include` 为准（先取白名单，再从中剔除 `exclude`）。为避免歧义，推荐**只使用其中一种**：

- 需要严格收敛能力 → 用 `include`。
- 仅需屏蔽个别高危工具 → 用 `exclude`。

---

## 7. 自定义 MCP Server 开发指引

任何符合 MCP 规范的 Server 都能接入 chat-runtime。开发一个自定义 Server 的基本步骤：

### 7.1 选择 SDK 与传输

- 使用官方或社区 MCP SDK（如 TypeScript、Python、Go 等）实现 Server。
- 选择传输方式：本地工具用 `stdio`；远程服务用 `streamable-http`（推荐）或 `sse`。

### 7.2 实现工具

一个工具需要提供：

1. **名称**（name）：简洁、语义化，如 `get_pods`。注册到 chat-runtime 后会自动加 `<server>__` 前缀。
2. **描述**（description）：清晰说明工具用途、何时使用——这直接影响 LLM 是否/如何调用它。
3. **输入 Schema**（JSON Schema）：定义参数结构、类型、必填项与约束。Schema 越精确，模型越不易传错参数。
4. **执行逻辑**：接收参数、执行操作、返回结构化结果或错误。

### 7.3 设计原则

- **单一职责**：每个工具只做一件事，避免"万能工具"。
- **只读与写操作分离**：便于在 chat-runtime 侧对写操作单独要求审批。
- **明确的错误返回**：以清晰的错误信息返回失败，帮助模型自我纠正。
- **幂等性**：尽量让工具可安全重试。
- **参数校验**：在 Server 内部校验参数，不要完全依赖模型传参正确。

### 7.4 接入 chat-runtime

实现完成后，在配置中声明即可：

```yaml
mcp_servers:
  my-server:
    transport: stdio          # 或 streamable-http / sse
    command: ./my-mcp-server
    auto_approve:
      - safe_read_tool        # 只读工具自动批准
    exclude:
      - internal_debug_tool   # 屏蔽不希望模型使用的工具
```

启动 chat-runtime 后，其工具会以 `my-server__<tool>` 形式出现在对话中。

### 7.5 联调建议

- 先用 `auto_approve: false` 保持全部审批，观察模型如何调用工具、传了哪些参数。
- 确认工具描述与 Schema 是否让模型正确理解用途，据此迭代描述文案。
- 稳定后再将只读工具设为 `auto_approve`。

---

## 8. 常见问题

**Q1：MCP 工具没有出现在对话里？**
- 检查 `mcp_servers` 是否正确声明，`transport` 与地址/命令是否可用。
- stdio 模式确认 `command` 路径正确、可执行且能正常启动。
- 若配置了 `include`，确认目标工具在白名单内；若配了 `exclude`，确认没有误伤。
- 若在 `chats.<name>.mcp_servers` 中限定了启用的 Server，确认目标 Server 在列表内。

**Q2：调用工具时一直卡在审批？**
- 这是预期行为：未被 `auto_approve` 的工具需要人工确认。CLI 下按提示确认，Web 下在审批弹窗中操作。
- 若希望自动放行只读工具，将其原始名加入 `auto_approve` 列表。

**Q3：stdio Server 启动失败？**
- 确认可执行权限（`chmod +x`）与运行依赖。
- 检查 `env` 是否注入了必要的环境变量（如 `KUBECONFIG`）。
- 使用 `${VAR}` 引用环境变量时，确认部署环境已设置对应变量。

**Q4：远程 Server 报鉴权失败？**
- 检查 `headers` 中的鉴权信息（如 `Authorization: Bearer ${MCP_TOKEN}`）。
- 确认 `MCP_TOKEN` 等环境变量已正确设置且未过期。

**Q5：工具名冲突了怎么办？**
- 不会冲突。所有 MCP 工具都带 `<server_name>__` 前缀，不同 Server 的同名工具会被自然区分。

**Q6：并发调用导致 Server 出错？**
- 对整体非并发安全的 Server 设置 `no_concurrent: true`。
- 对个别写操作使用 `no_concurrent_tools` 精确串行化。

**Q7：如何减少 MCP 工具占用的上下文？**
- 用 `include` 只暴露当前对话真正需要的工具。
- 在 `chats` 中仅启用相关的 `mcp_servers`。
- 精简 Server 侧的工具描述，保持信息量的同时避免冗长。

---

## 9. 参考

- 配置详解：[configuration.md](configuration.md)
- 架构与安全设计：[architecture.md](architecture.md)
- MCP 协议规范：[Model Context Protocol](https://modelcontextprotocol.io/)
