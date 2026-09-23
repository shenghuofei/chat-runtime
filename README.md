# Chat-Runtime

[English](#english) | [中文](#中文)

<a id="中文"></a>

一个轻量、可嵌入的 LLM Agent 运行时框架。单一二进制，开箱即用。

## 特性

- 🧠 **多 Provider 支持** — OpenAI / Claude / DeepSeek / Ollama / 火山引擎(Ark) / Gemini / Qwen，通过统一接口切换
- 🔧 **MCP 协议集成** — 完整的 MCP Client 实现，支持 stdio / SSE / Streamable HTTP 三种传输
- 🛡️ **Human-in-the-loop** — 工具调用审批机制，危险操作需用户确认
- 📦 **单一二进制** — 前端 + 后端 + 静态资源全部编译进一个可执行文件
- 💾 **会话持久化** — checkpoint + 增量日志，重启恢复上下文
- 📊 **上下文管理** — 三种溢出策略：compress（摘要压缩）/ truncate（截断）/ window（token 窗口）
- 🌐 **双模式** — CLI 交互 + WebSocket Web 服务
- 🔌 **插件化工具** — 内置工具 + MCP Server + 自定义工具，统一接口
- ⚡ **Prompt Cache 优化** — 消息标准化、工具排序，最大化 LLM 缓存命中率

## 快速开始

### 安装

```bash
# 从源码构建
git clone https://github.com/YOUR_USERNAME/chat-runtime.git
cd chat-runtime
make build

# 或直接 go install
go install github.com/YOUR_USERNAME/chat-runtime@latest
```

### 最小配置

```yaml
# config.yml
providers:
  deepseek:
    type: deepseek
    api_key: sk-your-key

models:
  default:
    provider: deepseek
    model: deepseek-chat

chats:
  default:
    model: default
    system: "你是一个有用的助手。"
```

或者直接复制包含所有参数（含默认值）的完整配置模板：

```bash
cp examples/config-default.yml config.yml
# 编辑 config.yml，填入你的 API Key 和模型名
```

更多 Provider 配置示例见 `examples/provider-*.yml`。

### 运行

```bash
# CLI 交互模式
./chat-runtime --config config.yml

# Web 服务模式
./chat-runtime serve --config config.yml --port 8080

# 一次性任务
./chat-runtime --config config.yml --once "列出当前目录的文件"
```

## 架构

```
┌──────────────────────────────────────────────────┐
│                  Transport 层                      │
│   CLI (readline)  │  WebSocket (gorilla/ws)        │
└────────┬─────────────────────┬───────────────────┘
         │                     │
┌────────▼─────────────────────▼───────────────────┐
│                   Agent 层                         │
│   Session 管理 │ Tool Calling 循环 │ 审批流          │
└────────┬────────────┬────────────────┬───────────┘
         │            │                │
┌────────▼────┐ ┌─────▼──────┐ ┌──────▼──────────┐
│  Provider   │ │  Manager   │ │     Tools        │
│ 多 LLM 适配  │ │ 上下文管理  │ │ 内置 + MCP + 自定义│
└─────────────┘ └────────────┘ └─────────────────┘
         │
┌────────▼────┐
│   Store     │
│  持久化存储   │
└─────────────┘
```

## 配置说明

详见 [docs/configuration.md](docs/configuration.md)

## MCP Server 集成

```yaml
mcp_servers:
  k8s-eye:
    transport: stdio
    command: ./k8s-eye
    env:
      KUBECONFIG: ~/.kube/config
    auto_approve: true   # 所有工具自动批准

  web-search:
    transport: sse
    url: https://your-mcp-host/sse
    auto_approve:
      - search   # 只自动批准 search 工具
```

详见 [docs/mcp.md](docs/mcp.md)

## 开发

```bash
# 安装依赖
go mod download

# 运行测试
make test

# 运行 lint
make lint

# 构建
make build

# 构建所有平台
make build-all
```

## 项目结构

```
chat-runtime/
├── cmd/                    # 入口
│   ├── root.go             # CLI 交互模式
│   └── serve.go            # Web 服务模式
├── pkg/
│   ├── agent/              # Agent 编排核心
│   │   ├── agent.go        # Agent 主循环
│   │   ├── session.go      # 会话生命周期
│   │   └── approval.go     # 工具审批
│   ├── config/             # 配置解析
│   │   └── config.go       # YAML 配置 + 校验
│   ├── manager/            # 上下文管理
│   │   └── manager.go      # compress/truncate/window
│   ├── mcp/                # MCP 客户端
│   │   ├── client.go       # MCP Client 封装
│   │   └── tools.go        # MCP 工具发现与注册
│   ├── provider/           # LLM Provider
│   │   ├── factory.go      # Provider 工厂
│   │   └── types.go        # 统一接口定义
│   ├── server/             # Web 服务
│   │   └── server.go       # WebSocket + 静态文件
│   ├── store/              # 持久化
│   │   └── store.go        # checkpoint + JSONL
│   ├── tools/              # 内置工具
│   │   ├── command.go      # 命令执行
│   │   └── filesystem.go   # 文件操作
│   └── web/                # 前端资源
│       ├── embed.go        # go:embed
│       └── static/         # HTML/CSS/JS
├── docs/                   # 文档
│   ├── architecture.md     # 架构设计
│   ├── configuration.md    # 配置详解
│   └── mcp.md              # MCP 集成指南
├── examples/               # 示例配置
├── Makefile
├── Dockerfile
└── README.md
```

## License

Apache-2.0

---

<a id="english"></a>

A lightweight, embeddable LLM Agent runtime framework. Single binary, batteries included.

## Features

- 🧠 **Multi-Provider** — OpenAI / Claude / DeepSeek / Ollama / Ark / Gemini / Qwen via unified interface
- 🔧 **MCP Protocol** — Full MCP Client with stdio / SSE / Streamable HTTP transports
- 🛡️ **Human-in-the-loop** — Tool approval mechanism for dangerous operations
- 📦 **Single Binary** — Frontend + backend + assets compiled into one executable
- 💾 **Session Persistence** — Checkpoint + incremental log, survives restarts
- 📊 **Context Management** — Three overflow strategies: compress / truncate / window
- 🌐 **Dual Mode** — CLI interactive + WebSocket web server
- 🔌 **Pluggable Tools** — Built-in + MCP Server + custom tools via unified interface
- ⚡ **Prompt Cache Optimization** — Message normalization and tool ordering for maximum cache hits

## Quick Start

See the Chinese section above for detailed instructions — the commands are the same.

## License

Apache-2.0
