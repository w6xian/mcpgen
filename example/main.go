// Command example 是一个最小的 sloth client：把 v1.HelloService 的导出方法
// 注册公开为 v1.Xxx，并按 MCP 约定挂上 meta 能力清单服务。
//
// 运行（先起 rpc-proxy，sloth 监听默认 8967）：
//
//	go run ./example -addr 127.0.0.1:8967 -app <app_id> -secret <secret>
//
// 注册成功后（{name} 是 apps/*.toml 里的 name，不是 app_id）：
//
//	POST http://127.0.0.1:8080/rpc/{name}/v1/hello   -d '{}'
//	POST http://127.0.0.1:8080/rpc/{name}/v1/echo    -d '{"message":"hi","upper":true}'
//	GET  http://127.0.0.1:8080/rpc/{name}/$tools        ← MCP 能力清单
package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/w6xian/sloth/v4"
	"github.com/w6xian/tlv"
)

func main() {
	var (
		addr   = flag.String("addr", "127.0.0.1:8967", "rpc-proxy 的 sloth 监听地址")
		appID  = flag.String("app", "", "app_id（apps/*.toml 里那个，Reg 报到用）")
		secret = flag.String("secret", "", "app_secret")
	)
	flag.Parse()
	if *appID == "" || *secret == "" {
		sloth.Errorw(context.Background(), "missing credentials",
			"hint", "用 -app / -secret 传入 apps/*.toml 里的 app_id 与 secret")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := sloth.DefaultClient()
	client.Header.Set("app_id", *appID)
	client.Header.Set("app_secret", *secret)
	conn := sloth.ClientConn(client)

	// ① 把 v1.Xxx 注册公开：HTTP 侧 /rpc/{name}/v1/hello → v1.Hello。
	if err := conn.Register("v1", &HelloService{}, ""); err != nil {
		sloth.Errorw(ctx, "register v1 service failed", "err", err)
		os.Exit(1)
	}
	// ② MCP 注册格式：meta.Tools 回报能力清单，代理 Reg 成功后下行拉取一次。
	//    tools_gen.go 由 mcpgen 从方法注释生成（go generate ./example）。
	if err := conn.Register("meta", &MCPMetaService{}, "能力清单"); err != nil {
		sloth.Errorw(ctx, "register meta service failed", "err", err)
		os.Exit(1)
	}

	if err := conn.Dial(ctx, sloth.TCP, *addr); err != nil {
		sloth.Errorw(ctx, "dial failed", "addr", *addr, "err", err)
		os.Exit(1)
	}
	defer conn.Close()

	raw, err := client.Call(ctx, "v1.Reg", []byte(`{"remark":"mcpgen example"}`))
	if err != nil {
		sloth.Errorw(ctx, "reg failed", "err", err)
		os.Exit(1)
	}
	var reg struct {
		Name     string `json:"name"`
		AppId    string `json:"app_id"`
		UserId   int64  `json:"user_id"`
		CallPath string `json:"call_path"`
	}
	if err := json.Unmarshal(tlv.Value(raw), &reg); err != nil {
		sloth.Errorw(ctx, "decode reg response failed", "err", err)
		os.Exit(1)
	}
	sloth.Infow(ctx, "registered", "name", reg.Name, "app_id", reg.AppId, "user_id", reg.UserId,
		"hello", "POST "+reg.CallPath+"v1/hello", "$tools", "GET "+reg.CallPath+"$tools")

	// 心跳：对齐 rpc-proxy 的 [registry].ping_timeout 巡检（20s 一跳）。
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				callCtx, cancelCall := context.WithTimeout(ctx, 5*time.Second)
				_, _ = client.Call(callCtx, "v1.Ping", nil) // 断线期间失败属正常
				cancelCall()
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	sloth.Infow(ctx, "example stopped", "name", reg.Name)
}
