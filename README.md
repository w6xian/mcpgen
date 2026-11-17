# mcpgen

从 Go 源码注释生成 MCP 能力清单（`tools_gen.go`）的构建期工具。

它服务于这样一条链路：

```
sloth client（内网服务提供方）
  ├─ conn.Register("v1", &HelloService{}, "")   ← 方法对外公开为 v1.Xxx
  └─ conn.Register("meta", &MCPMetaService{}, "") ← 能力清单服务（本工具生成）
                    │
                    ▼  Reg 成功后代理下行调 meta.Tools 拉取一次
rpc-proxy（注册中心 + HTTP 代理 + MCP 网关）
  ├─ GET  /rpc/{name}/$tools     ← 人看的清单
  ├─ POST /mcp  tools/list       ← AI 看的工具清单（自动转成 MCP tool）
  └─ POST /rpc/{name}/{svc}/{method}  ← 真正的调用
```

一句话：**方法注释是声明的唯一来源，mcpgen 在构建期把它编译进一个自包含的 Go 文件，运行时不依赖本工具。**

## 为什么是构建期生成

注释不会进编译产物，运行时反射拿不到。所以提取只能在 `go generate` 阶段做：用 `go/parser` 解析指定目录，找出符合 sloth 约定的 RPC 方法，从 doc comment 里抠出声明，生成一个**只用标准库**的 `tools_gen.go`（`MCPTools` 清单 + `MCPMetaService.Tools`）。产物提交进仓库，改了注释/方法后重新生成。

## 安装

作为 Go tool 固定在消费方仓库（Go 1.24+ 的 `tool` 指令，版本锁定、不随 `go get` 漂移）：

```bash
go get -tool github.com/w6xian/mcpgen@v0.0.2
```

也可以直接跑当前源码：

```bash
go run github.com/w6xian/mcpgen -dir ./cmd/sloth -service receipt
```

## 用法

参数：

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-dir` | `.` | 要解析的包目录（服务方法的 `.go` 所在目录） |
| `-service` | 必填 | 默认 service 段；方法注释里写 `service: xxx` 可按方法覆盖 |
| `-out` | `<dir>/tools_gen.go` | 输出文件路径 |
| `-pkg` | 沿用被解析包 | 产物的 package 名 |
| `-check` | off | **只校验不写出**：重新生成并与现有产物比对，不一致退出码 1（CI 卡点） |

推荐在包里放一行 `//go:generate`，之后统一 `go generate`：

```go
//go:generate go tool mcpgen -dir . -service v1
```

消费侧再挂上清单服务（生成产物里自带 `MCPMetaService`）：

```go
conn.Register("v1", &HelloService{}, "")
conn.Register("meta", &MCPMetaService{}, "能力清单")
```

## 注释约定

写在方法的 doc comment 里，逐行识别前缀，大小写不敏感：

```go
// ToJson 把 PDF 回单解析成 JSON。            ← 无前缀首行 = summary
// when: 用户上传回单需要结构化入库时调用       ← 何时调（给 AI 的软提示）
// service: receipt                          ← 可选：覆盖 -service（一个包里有多个服务段时用）
// input: ReceiptRequest                     ← 指向同包结构体，schema 从 json tag 推导
// input_schema: {"type":"object",...}        ← 或：直接手写完整 schema（优先于 input:）
// example: {"data":"<base64>"}              ← 示例请求体
func (h *ReceiptService) ToJson(ctx context.Context, b []byte) ([]byte, error)
```

| 字段 | 来源 | 说明 |
| --- | --- | --- |
| `summary` | 无前缀首行，或 `summary:` | 一句话说明方法干什么 |
| `when` | `when:` | 调用条件，只作提示，不参与鉴权/路由 |
| `service` | `service:` | 覆盖命令行 `-service`，按方法指定服务段 |
| `input_schema` | `input:` 推导，或 `input_schema:` 手写 | 入参 JSON Schema，见下 |
| `example` | `example:` | 示例请求体，原样透传 |

> 识别规则：声明行的 key 是冒号前的**单个词**（不含空格）。散文句子里出现
> "example:" 这类词不会误判——冒号前含空格的行按散文处理，首条散文照常当 summary。

### schema 的两种来源

**1. 从结构体推导（`input:`）**

- 无 `omitempty` 的字段进 `required`；
- `mcp:"说明"` tag 变成该字段 schema 的 `description`；
- 类型映射：`string`→string、整型→integer、浮点→number、`bool`→boolean、`[]byte`→`{type:string, contentEncoding:base64}`、`time.Time`→`{type:string, format:date-time}`、数组/匿名结构体/map 递归展开。

```go
type EchoRequest struct {
    Message string `json:"message" mcp:"要回显的文本"`
    Upper   bool   `json:"upper,omitempty" mcp:"是否转成大写"`
}
// → {"properties":{"message":{"description":"要回显的文本","type":"string"},
//                  "upper":{"description":"是否转成大写","type":"boolean"}},
//    "required":["message"],"type":"object"}
```

**2. 手写覆盖（`input_schema:`）**

嵌套对象、外部包类型等推导够不到的场景，直接写完整 schema，生成器**原样记入清单、不再推导**。规则：

- 必须是 JSON **对象**（`true`、`[]` 这类合法 JSON 但不是对象的会被告警并忽略）；
- 可跨行：从该行起逐行拼接，直到整体是合法 JSON；**字符串值不可拆到两行**（拼接会改变内容）；
- 拼接途中遇到下一条声明（`when:` / `example:` …）还没拼合法就停手告警，**不吞后续声明**。

```go
// input_schema: {"type":"object","properties":{
// "name":{"type":"string","description":"姓名"},
// "address":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}},
// "required":["name"]}
```

## 完整示例

`example/` 是一个可运行的 sloth client，暴露 `v1.Hello` / `v1.Echo` / `v1.Greet`：

```bash
# 重新生成清单（改了注释后执行）
go generate ./example

# 连上 rpc-proxy 运行
go run ./example -addr 127.0.0.1:8967 -app <app_id> -secret <secret>
```

生成的 `example/tools_gen.go` 片段（`Greet` 用手写嵌套 schema）：

```go
var MCPTools = []MCPToolDecl{
    {
        Service: "v1", Method: "Greet",
        Summary: "Greet 按姓名和地址问候：演示 input_schema 手写嵌套结构…",
        InputSchema: `{"type":"object","properties":{"name":{...},"address":{...}},"required":["name"]}`,
        Example: `{"name":"张三","address":{"city":"深圳"}}`,
    },
    // ...
}

type MCPMetaService struct{}
func (MCPMetaService) Tools(ctx context.Context, _ []byte) ([]byte, error) {
    return json.Marshal(map[string]any{"schema_version": 1, "tools": MCPTools})
}
```

注册成功后，调用方侧：

```bash
POST /rpc/{name}/v1/hello   -d '{}'
POST /rpc/{name}/v1/echo    -d '{"message":"hi","upper":true}'
GET  /rpc/{name}/$tools                     # 人看的清单
POST /mcp  {"method":"tools/list"}          # AI 看的工具清单
```

## 边界与限制

**方法识别**

- 只收**导出类型上的导出方法**，且签名第一参数是 `context.Context`、第二参数是 `[]byte`、恰好两个返回值（末位 `error`）——这是 sloth 的约定，不符合的方法静默跳过。
- `service` 默认来自命令行 `-service`，方法注释 `service: xxx` 可按方法覆盖；方法名就是 Go 的导出方法名（首字母大写），HTTP 路径里首字母自动大写是对齐这一点的。
- 带 `// Code generated` 标记的文件会被跳过（只认 package 子句之前的标记，正文注释不算），清单自身不会被收进清单。

**schema 推导的精度上限**

- 外部包类型（如 `[]bankreceipt.Receipt`）只能压成 `{"type":"object","description":"外部类型 ..."}`，嵌套字段看不见；用 `input_schema:` 手写，或把类型挪进同包。
- 嵌入字段直接跳过；没有 `$schema` 声明。
- 这些是静态推导的精度问题，不是格式错误；MCP 要求 `inputSchema` 是 `type:object` 的 JSON Schema，产物满足。
- 手写 `input_schema` 校验只到"整体是合法 JSON"，不校验是否符合 JSON Schema 规范——写错不会被拦，需自查。

**职责边界**

- 清单里的 `when` / `example` / 描述**只是给调用方（尤其 LLM）看的软提示**，不参与鉴权与路由。真正的门禁是 rpc-proxy 侧的 `allow` 和白名单头。
- 生成器不注册服务、不建连接、不参与运行时。挂 `meta` 服务是消费方代码的事；没挂的话代理按"未实现"降级（WARN 一条，不影响注册与调用）。
- 能力清单是"自报"的：同一个 `name` 多实例声明不一致时代理会打 WARN，以后到的为准；代理侧对条数/长度有上限（200 条、单字段 4KB、整包 512KB）。

**漂移检查不阻断**

以下情况只打 WARN、仍生成产物：方法没有 doc comment（清单里空描述）、`input:` 指向的未知结构体、`input_schema` 非对象或拼不出合法 JSON。提交前看一眼 stderr。

**CI 卡点（`-check`）**

"改了注释没重新生成" 可以变成 CI 硬卡点：CI 里跑 `mcpgen -check -dir . -service xxx`（或 `go tool mcpgen -check ...`），产物与注释不一致就退出码 1。校验是"重新生成后逐字节比对"，同输入必同输出，不会误报。

**版本落地**

生成器是 tool，只在 `go generate` 时起作用；发布新版**不会**让已有仓库的 `tools_gen.go` 变化。要应用新能力，需在消费方 bump `tool` 版本后重新生成并提交产物。本地临时指向未发布源码可用 `go.work`（放仓库外、不提交）或 `go.mod replace`（用完还原，别提交指向本机路径的 replace）。

## 开发

```bash
go build ./... && go vet ./... && go test ./...
go generate ./example    # 重新生成示例清单
```

测试用 golden 文件（`testdata/sample/expected_tools_gen.go`）锁定行为：
同输入必同输出，改了生成逻辑跑 `go test`，产物变化会直接比对失败。
