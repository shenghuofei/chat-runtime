# feat: 多会话侧边栏 — 新建、切换、保留历史

## 需求描述

实现类似 ChatGPT 左侧的多会话侧边栏，支持在多个对话之间自由切换，每个对话保留独立的历史记录。

## 当前状态

- ✅ 后端 Store 已支持按 session 持久化（checkpoint + JSONL 增量日志）
- ✅ 前端有"新对话"按钮，可以开启新 session
- ❌ 前端没有会话列表，无法查看和切回历史会话
- ❌ 后端没有列出会话的 API
- ❌ WebSocket 连接建立时不会推送历史消息到前端

## 需要实现

### 后端

| 改动 | 说明 |
|------|------|
| `Store.ListSessions()` 方法 | 扫描 `~/.chat-runtime/sessions/` 目录，返回会话列表（ID、chat preset、最后活跃时间、首条消息摘要） |
| `GET /api/sessions?chat=xxx` | 列出指定 chat preset 下的所有会话 |
| `DELETE /api/sessions/:id` | 删除指定会话（文件 + 内存） |
| WebSocket 连接时推送历史 | 建立连接后，如果 session 已有历史，把消息推送给前端渲染 |

### 前端

| 改动 | 说明 |
|------|------|
| 左侧侧边栏 UI | 会话列表，每条显示摘要 + 时间 |
| 新建按钮 | 移到侧边栏顶部 |
| 点击切换 | 切换 sessionId → 重连 WebSocket → 接收历史 → 渲染 |
| 删除按钮 | 每条会话右侧，调用 DELETE API |
| 响应式 | 移动端可折叠侧边栏 |

### 参考

- ChatGPT / Claude 的左侧会话列表交互
- chat-agent 项目的 Web 多会话实现
