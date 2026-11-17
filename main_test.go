package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestGenerateGolden 端到端：解析 testdata/sample 并与基准产物逐字节比对。
// 产物变化必须显式更新 expected_tools_gen.go——这正是给消费方承诺的
// "同输入同输出"（-check 模式在消费方仓库里依赖这一点）。
func TestGenerateGolden(t *testing.T) {
	code, n, warns, err := generate("testdata/sample", "v1", "")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if n != 4 {
		t.Fatalf("应解析出 4 条声明，得 %d", n)
	}
	// 预期告警：Bad 的非对象 schema、Broken 的多行拼不合法
	if len(warns) != 2 {
		t.Fatalf("应恰好 2 条告警（Bad 非对象 / Broken 拼不合法），得 %d 条:\n%s",
			len(warns), strings.Join(warns, "\n"))
	}
	golden, err := os.ReadFile("testdata/sample/expected_tools_gen.go")
	if err != nil {
		t.Fatalf("读 golden: %v", err)
	}
	if !bytes.Equal(code, golden) {
		t.Fatalf("产物与 golden 不一致。\n--- got ---\n%s\n--- want ---\n%s", code, golden)
	}
}

// TestGenerateDetails 产物内容断言：每条注释约定都要落进产物。
func TestGenerateDetails(t *testing.T) {
	code, _, _, err := generate("testdata/sample", "v1", "")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	s := string(code)

	// service: 覆盖 -service
	if !strings.Contains(s, `"greet"`) {
		t.Fatal("Greet 的 service 覆盖丢失")
	}
	// 多行 input_schema 拼接合法且被记录（required 在其中）
	if !strings.Contains(s, `required`) || !strings.Contains(s, `name`) {
		t.Fatal("多行 schema 没进产物")
	}
	// 非对象 schema 被忽略：Bad 不应带 InputSchema（裸 true 不出现）
	if strings.Contains(s, `InputSchema: "true"`) {
		t.Fatal("非对象 input_schema 不应进产物")
	}
	// 多行拼不合法时不吞后续声明：Broken 的 example 还在
	if !strings.Contains(s, `{\"y\":2}`) {
		t.Fatal("Broken 的 example 被吞了")
	}
	// 散文含冒号不影响 summary
	if !strings.Contains(s, "Broken 多行 schema 永远拼不合法") {
		t.Fatal("含冒号散文的 summary 丢失")
	}
	// 推导路径：json:"-" 字段被剔除
	if strings.Contains(s, `"debug"`) {
		t.Fatal(`json:"-" 字段不应进 schema`)
	}
	// mcp tag 变成 description
	if !strings.Contains(s, "姓名") {
		t.Fatal("mcp tag 的 description 丢失")
	}
}

// TestSplitKvProse 含冒号的散文不当 key（回归：summary 曾被吞）。
func TestSplitKvProse(t *testing.T) {
	key, val := splitKv("撞上 example: 声明即停手")
	if key != "" || val != "" {
		t.Fatalf("散文应返回空 key: %q / %q", key, val)
	}
	key, val = splitKv("when: 正常声明")
	if key != "when" || val != "正常声明" {
		t.Fatalf("声明拆分不对: %q / %q", key, val)
	}
	key, _ = splitKv("无冒号散文")
	if key != "" {
		t.Fatalf("无冒号应返回空 key: %q", key)
	}
}

// TestValidSchemaObject 顶层必须是对象。
func TestValidSchemaObject(t *testing.T) {
	for val, want := range map[string]bool{
		`{"type":"object"}`: true,
		`{}`:                true,
		`true`:              false,
		`[]`:                false,
		`"str"`:             false,
		`{"type":"object"`:  false, // 未闭合
	} {
		if got := validSchemaObject(val); got != want {
			t.Errorf("validSchemaObject(%s) = %v, want %v", val, got, want)
		}
	}
}
