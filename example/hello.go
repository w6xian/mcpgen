package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

//go:generate go run .. -dir . -service v1

// EchoRequest Echo 的入参；mcpgen 从 json tag 推导 input_schema。
type EchoRequest struct {
	Message string `json:"message" mcp:"要回显的文本"`
	Upper   bool   `json:"upper,omitempty" mcp:"是否转成大写"`
}

// HelloService 示例服务：在 sloth client 里注册成 v1，方法对外就是 v1.Xxx。
// 方法签名遵循 sloth 约定：(context.Context, []byte)（一或两个返回值，末位为 error）；
// mcpgen 收的是导出类型上的导出方法 + 恰好两个返回值。
// doc comment 遵循 mcpgen 约定：首行 summary，when:/input:/input_schema:/example: 为补充字段。
type HelloService struct{}

// Hello 说声你好：验证 v1.Hello 能被代理发现并调用。
// when: 想快速确认链路是否打通时调用；无入参
// example: {}
func (*HelloService) Hello(_ context.Context, _ []byte) ([]byte, error) {
	return []byte(`{"message":"hello from v1.Hello"}`), nil
}

// Echo 把消息回显，upper=true 时转成大写。
// when: 需要确认请求体是否被原样透传时调用
// input: EchoRequest
// example: {"message":"hello","upper":true}
func (*HelloService) Echo(_ context.Context, b []byte) ([]byte, error) {
	var req EchoRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, fmt.Errorf("invalid EchoRequest: %w", err)
	}
	if req.Upper {
		req.Message = strings.ToUpper(req.Message)
	}
	return json.Marshal(map[string]string{"echo": req.Message})
}

// Greet 按姓名和地址问候：演示 input_schema 手写嵌套结构（外部包类型/深嵌套推导不够用时）。
// when: 需要结构化入参、且字段来自别的包或多层嵌套时调用
// input_schema: {"type":"object","properties":{"name":{"type":"string","description":"姓名"},"address":{"type":"object","properties":{"city":{"type":"string"},"zip":{"type":"string"}},"required":["city"]}},"required":["name"]}
// example: {"name":"张三","address":{"city":"深圳","zip":"518000"}}
func (*HelloService) Greet(_ context.Context, b []byte) ([]byte, error) {
	var req struct {
		Name    string `json:"name"`
		Address struct {
			City string `json:"city"`
			Zip  string `json:"zip,omitempty"`
		} `json:"address,omitempty"`
	}
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, fmt.Errorf("invalid GreetRequest: %w", err)
	}
	return json.Marshal(map[string]string{"greeting": "hello " + req.Name + " from " + req.Address.City})
}
