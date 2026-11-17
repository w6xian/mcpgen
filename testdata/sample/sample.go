// sample 是 golden 测试用的假想服务包：覆盖全部注释约定与告警路径。
//
// 本目录不会被任何构建引用，仅被 mcpgen 的测试解析。
package sample

import "context"

// Req 推导来源。
type Req struct {
	Name  string `json:"name" mcp:"姓名"`
	Age   int    `json:"age,omitempty"`
	Debug bool   `json:"-"`
}

// Hello 常规推导：input 指向 Req，生成 schema。
// when: 常规路径
// input: Req
// example: {"name":"a"}
func (*HelloService) Hello(ctx context.Context, b []byte) ([]byte, error) { return b, nil }

// Greet service 覆盖 + 多行 input_schema（含字符串不跨行的多行 JSON）。
// service: greet
// input_schema: {"type":"object","properties":{
// "name":{"type":"string","description":"姓名"}},
// "required":["name"]}
// example: {"name":"a"}
func (*HelloService) Greet(ctx context.Context, b []byte) ([]byte, error) { return b, nil }

// Bad input_schema 是合法 JSON 但不是对象：告警并忽略，example/when 不受影响。
// input_schema: true
// example: {"x":1}
// when: 应保留
func (*HelloService) Bad(ctx context.Context, b []byte) ([]byte, error) { return b, nil }

// Broken 多行 schema 永远拼不合法：撞上 example: 声明即停手，不吞后续声明。
// input_schema: {"type":"object",
// this is not json
// example: {"y":2}
func (*HelloService) Broken(ctx context.Context, b []byte) ([]byte, error) { return b, nil }
