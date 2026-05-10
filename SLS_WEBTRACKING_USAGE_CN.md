# SLS WebTracking 使用量上报

CLIProxyAPI 支持把每次模型请求的使用量统计通过阿里云 SLS WebTracking 上报到指定 Logstore。该功能默认关闭，需要在 `config.yaml` 中显式开启。

> 目标 Logstore 必须在阿里云 SLS 控制台开启 WebTracking。WebTracking 是匿名写入方式，请只发送可接受公开写入风险的数据。

## 配置示例

```yaml
sls-webtracking:
  enabled: true
  project: "your-sls-project"
  logstore: "your-logstore"
  endpoint: "cn-hangzhou.log.aliyuncs.com"
  topic: "cliproxy_usage"
  source: ""
  include-api-key: false
  queue-size: 1024
```

## 配置项

| 字段 | 类型 | 默认值 | 必填 | 说明 |
| --- | --- | --- | --- | --- |
| `enabled` | bool | `false` | 否 | SLS WebTracking 上报开关。默认关闭。 |
| `project` | string | `""` | 开启时必填 | SLS Project 名称。 |
| `logstore` | string | `""` | 开启时必填 | 目标 Logstore 名称。 |
| `endpoint` | string | `""` | 开启时必填 | SLS Endpoint，可写 `cn-hangzhou.log.aliyuncs.com`，也可写带 `https://` 的完整 endpoint。 |
| `topic` | string | `cliproxy_usage` | 否 | 写入 SLS 的 `__topic__`。 |
| `source` | string | 当前主机名 | 否 | 写入 SLS 的 `__source__`。留空时使用 hostname，取不到时使用 `cliproxyapi`。 |
| `include-api-key` | bool | `false` | 否 | 是否额外上报明文客户端 API key。默认关闭；建议优先使用 `api_key_hash` 统计用量。 |
| `queue-size` | int | `1024` | 否 | 本地异步发送队列大小。队列满时丢弃新的 SLS 上报记录，不阻塞业务请求。 |

## WebTracking 请求格式

每次发送到 SLS 的请求体形如：

```json
{
  "__topic__": "cliproxy_usage",
  "__source__": "cliproxyapi",
  "__logs__": [
    {
      "event": "usage",
      "provider": "codex",
      "model": "gpt-5-codex",
      "api_key_hash": "581d30b4caecfdbad68803e06cde0f826b06850314fbadc988d9ad6d0d4d3298",
      "total_tokens": "123"
    }
  ]
}
```

实际发送 URL：

```text
https://<project>.<endpoint>/logstores/<logstore>/track
```

例如：

```text
https://your-sls-project.cn-hangzhou.log.aliyuncs.com/logstores/your-logstore/track
```

## 上报字段

所有字段以字符串形式写入 SLS。

| 字段 | 示例 | 说明 |
| --- | --- | --- |
| `event` | `usage` | 固定事件类型。 |
| `request_time` | `2026-05-10T14:02:03.000000004Z` | 请求开始时间，UTC RFC3339Nano 格式。 |
| `request_id` | `req_xxx` | CLIProxyAPI 内部请求 ID；取不到时为空。 |
| `provider` | `codex` | 实际使用的 provider，例如 `codex`、`claude`、`gemini`、`openai-compatibility`。 |
| `model` | `gpt-5-codex` | 实际请求的上游模型名。 |
| `alias` | `codex-fast` | 客户端请求的模型别名；没有别名时等于 `model`。 |
| `endpoint` | `/v1/responses` | 下游请求 endpoint；取不到时为空。 |
| `source` | `user@example.com` | usage record 中解析出的来源，可能是账号、项目、email 或客户端 API key 来源；当来源等于客户端 API key 且未开启 `include-api-key` 时，会写入指纹而不是明文。 |
| `auth_id` | `auth-1` | 使用的 auth ID；取不到时为空。 |
| `auth_index` | `2` | 使用的 auth index；取不到时为空。 |
| `auth_type` | `oauth` | 认证类型，例如 `oauth`、`apikey`、`unknown`。 |
| `api_key_fingerprint` | `sk-1...7890` | 客户端 API key 指纹，不发送明文 API key。 |
| `api_key_hash` | `581d30b4caecfdbad68803e06cde0f826b06850314fbadc988d9ad6d0d4d3298` | 客户端 API key 的 SHA-256 十六进制哈希，可用于按 API key 聚合统计。取不到 API key 时为空。 |
| `api_key` | `sk-1234567890` | 明文客户端 API key。仅当 `sls-webtracking.include-api-key: true` 时上报；默认不包含该字段。 |
| `latency_ms` | `150` | 从上游请求开始到记录 usage 的耗时，单位毫秒。 |
| `failed` | `false` | 请求是否失败。 |
| `status_code` | `200` | HTTP 状态码；成功默认 `200`，失败取错误状态，取不到时默认 `500`。 |
| `failure_body` | `usage limit reached` | 失败错误文本，最长 4096 字符；成功时通常为空。 |
| `input_tokens` | `10` | 输入 token 数。 |
| `output_tokens` | `20` | 输出 token 数。 |
| `reasoning_tokens` | `5` | 推理/思考 token 数。 |
| `cached_tokens` | `2` | 缓存命中的输入 token 数。 |
| `total_tokens` | `35` | 总 token 数。上游未提供时优先按输入、输出和推理字段推导；若这些字段均为 0，则再包含缓存字段推导。 |

## 不会上报的数据

| 数据 | 是否上报 | 说明 |
| --- | --- | --- |
| Prompt 内容 | 否 | 不发送用户输入内容。 |
| 模型响应正文 | 否 | 不发送模型输出内容。 |
| 请求头 | 否 | 不发送完整 headers。 |
| 明文客户端 API key | 默认否 | 默认只发送 `api_key_fingerprint` 和 `api_key_hash`；仅当 `sls-webtracking.include-api-key: true` 时发送 `api_key`。 |
| 上游 OAuth token | 否 | 不发送 access token、refresh token。 |
| 完整请求日志文件 | 否 | SLS 上报只发送 usage 字段。 |

## 异步发送行为

| 行为 | 说明 |
| --- | --- |
| 是否异步 | 是。业务请求完成后生成 usage record，SLS 插件写入本地 channel 队列，后台 goroutine 发送 HTTP 请求。 |
| 是否阻塞业务请求 | 不阻塞。SLS 网络失败、SLS 返回错误或队列满都不会影响模型请求返回。 |
| 队列满时 | 丢弃新的 SLS usage 记录，并写 debug 日志。 |
| 发送失败时 | 写 warning 日志，不重试。 |
| 热重载 | 支持。修改 `config.yaml` 后，文件 watcher 热重载会更新 SLS WebTracking 配置。 |
| 关闭开关 | 将 `sls-webtracking.enabled` 改为 `false` 后，插件停止接收新的上报记录。 |

## 和 `usage-statistics-enabled` 的关系

| 配置 | 作用 |
| --- | --- |
| `usage-statistics-enabled` | 控制内置 Redis 兼容 usage 队列/管理统计输出。 |
| `sls-webtracking.enabled` | 控制是否向阿里云 SLS WebTracking 上报 usage。 |

两者互相独立。即使 `usage-statistics-enabled: false`，只要 `sls-webtracking.enabled: true` 且配置完整，SLS WebTracking 仍会发送 usage 记录。

## 阿里云侧要求

| 项目 | 说明 |
| --- | --- |
| Logstore | 需要开启 WebTracking。 |
| Endpoint | 使用目标 Project 所在地域的 SLS Endpoint。 |
| 权限 | WebTracking 是匿名采集接口，不需要 AccessKey。 |
| 数据格式 | CLIProxyAPI 发送 `__topic__`、`__source__` 和 `__logs__`。 |

参考文档：

- [PutWebtracking 接口](https://help.aliyun.com/zh/sls/developer-reference/api-sls-2020-12-30-putwebtracking)
- [使用 WebTracking 采集日志](https://help.aliyun.com/zh/sls/use-the-web-tracking-feature-to-collect-logs)
