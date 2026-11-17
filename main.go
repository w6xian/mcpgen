// Command mcpgen 从 Go 源码注释生成能力清单（tools_gen.go）。
//
// 原理：注释在编译产物里不存在，所以提取只能在构建期做——本工具用 go/parser
// 解析指定目录，找出符合 sloth 约定的 RPC 方法（导出类型上的导出方法、
// 签名 (context.Context, []byte) + 两个返回值），从 doc comment 里提取声明，
// 生成一个自包含的 tools_gen.go（只用标准库），客户端把它注册成 meta 服务，
// 代理 Reg 成功后调用 meta.Tools 拉取。
//
// 注释约定（都写在方法的 doc comment 里，逐行识别前缀，大小写不敏感）：
//
//	// 把 PDF 回单解析成 JSON。          ← 第一行 = summary（无前缀的行）
//	// when: 用户上传回单需要结构化入库时调用
//	// input: ReceiptRequest            ← 指向同包结构体，schema 从 json tag 推导
//	// example: {"data":"<base64>"}
//	func (h *ReceiptService) ToJson(ctx context.Context, b []byte) ([]byte, error)
//
// required 判定：json tag 无 omitempty 即必填；`mcp:"说明"` tag 变成字段描述。
//
// 用法：
//
//	go run 路径/rpc-proxy/cmd/mcpgen -dir ./cmd/sloth -service receipt
//
// 产物提交进仓库；改了注释/方法后重新生成。生成时做漂移检查：
// 没写注释的 RPC 方法、input 指向的未知结构体，都会打 WARN（不阻断）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// toolDecl 生成产物里的一条声明（JSON 字段与代理侧 registry.ToolDecl 对齐）。
type toolDecl struct {
	Service     string `json:"service"`
	Method      string `json:"method"`
	Summary     string `json:"summary,omitempty"`
	When        string `json:"when,omitempty"`
	InputSchema string `json:"input_schema,omitempty"`
	Example     string `json:"example,omitempty"`
}

// rpcMethod 解析出来的一个 RPC 方法。
type rpcMethod struct {
	recv   string // 接收者类型名
	name   string // 方法名（线上就是它，sloth 只收录导出方法）
	doc    string // doc comment 原文（含换行）
	decl   *ast.FuncDecl
	summary, when, input, example string
}

func main() {
	var (
		dir     = flag.String("dir", ".", "要解析的包目录（客户端服务的 .go 所在目录）")
		service = flag.String("service", "", "默认 service 名（HTTP 路径里那段）；方法注释 service: 可覆盖")
		out     = flag.String("out", "", "输出文件（默认 <dir>/tools_gen.go）")
		pkg     = flag.String("pkg", "", "产物 package 名（默认沿用被解析包的）")
	)
	flag.Parse()
	if *service == "" {
		fatal("-service 必填：这是 HTTP 路径里的 service 段")
	}

	methods, structs, pkgName, err := parse(*dir)
	if err != nil {
		fatal("解析失败: %v", err)
	}
	if len(methods) == 0 {
		fatal("没找到任何 RPC 方法（导出类型上的导出方法，签名 (context.Context, []byte) + 两个返回值）")
	}

	var warns []string
	tools := make([]toolDecl, 0, len(methods))
	for _, m := range methods {
		parseDoc(m, &warns)
		t := toolDecl{
			Service: *service,
			Method:  m.name,
			Summary: m.summary,
			When:    m.when,
			Example: m.example,
		}
		if m.input != "" {
			st, ok := structs[m.input]
			if !ok {
				warns = append(warns, fmt.Sprintf("%s: input 指向的结构体 %q 不在同包内，跳过 schema 推导（外部包结构体请手写 example 或把类型挪进同包）", m.name, m.input))
			} else {
				schema := objectSchema(st, structs)
				raw, _ := json.Marshal(schema)
				t.InputSchema = string(raw)
			}
		}
		if t.Summary == "" {
			warns = append(warns, fmt.Sprintf("%s: 方法没有 doc comment，清单里将是空描述", m.name))
		}
		tools = append(tools, t)
	}
	sort.Slice(tools, func(i, j int) bool {
		if tools[i].Service != tools[j].Service {
			return tools[i].Service < tools[j].Service
		}
		return tools[i].Method < tools[j].Method
	})

	pkgOut := *pkg
	if pkgOut == "" {
		pkgOut = pkgName
	}
	code := render(pkgOut, tools)
	if err := os.WriteFile(outPath(*dir, *out), code, 0o644); err != nil {
		fatal("写出失败: %v", err)
	}
	fmt.Printf("mcpgen: %s 生成 %d 条声明（service=%s）\n", outPath(*dir, *out), len(tools), *service)
	for _, w := range warns {
		fmt.Fprintf(os.Stderr, "mcpgen: WARN %s\n", w)
	}
}

func fatal(f string, args ...any) {
	fmt.Fprintf(os.Stderr, "mcpgen: "+f+"\n", args...)
	os.Exit(1)
}

func outPath(dir, out string) string {
	if out != "" {
		return out
	}
	return filepath.Join(dir, "tools_gen.go")
}

// parse 解析目录：返回 RPC 方法列表、同包结构体索引、包名。
func parse(dir string) ([]*rpcMethod, map[string]*ast.StructType, string, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go") &&
			!strings.HasSuffix(fi.Name(), ".tmp")
	}, parser.ParseComments)
	if err != nil {
		return nil, nil, "", err
	}
	var (
		methods []*rpcMethod
		structs = map[string]*ast.StructType{}
		pkgName string
	)
	for _, pkg := range pkgs {
		pkgName = pkg.Name
		for _, file := range pkg.Files {
			// 跳过生成产物：tools_gen.go 里的 MCPMetaService.Tools 也长得像
			// RPC 方法，不跳过的话下次生成会把清单自身收录进清单
			if isGeneratedFile(file) {
				continue
			}
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok {
							continue
						}
						st, ok := ts.Type.(*ast.StructType)
						if ok {
							structs[ts.Name.Name] = st
						}
					}
				case *ast.FuncDecl:
					if m, ok := asRPCMethod(d); ok {
						methods = append(methods, m)
					}
				}
			}
		}
	}
	return methods, structs, pkgName, nil
}

// isGeneratedFile 文件头含 "Code generated"（go 生成约定标记）则跳过。
func isGeneratedFile(f *ast.File) bool {
	for _, c := range f.Comments {
		for _, cm := range c.List {
			if strings.Contains(cm.Text, "Code generated") {
				return true
			}
		}
	}
	return false
}

// asRPCMethod 判断一个 FuncDecl 是不是 sloth 风格的 RPC 方法：
// 导出类型上的导出方法，参数 (context.Context, []byte)，两个返回值。
func asRPCMethod(d *ast.FuncDecl) (*rpcMethod, bool) {
	if d.Recv == nil || len(d.Recv.List) == 0 || !d.Name.IsExported() {
		return nil, false
	}
	if len(d.Type.Params.List) != 2 || len(d.Type.Results.List) != 2 {
		return nil, false
	}
	if !isCtx(d.Type.Params.List[0].Type) || !isBytes(d.Type.Params.List[1].Type) {
		return nil, false
	}
	recv := "*"
	switch r := d.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := r.X.(*ast.Ident); ok && id.IsExported() {
			recv = id.Name
		}
	case *ast.Ident:
		if r.IsExported() {
			recv = r.Name
		}
	}
	if recv == "*" {
		return nil, false
	}
	doc := ""
	if d.Doc != nil {
		doc = d.Doc.Text()
	}
	return &rpcMethod{recv: recv, name: d.Name.Name, doc: doc, decl: d}, true
}

func isCtx(e ast.Expr) bool {
	if st, ok := e.(*ast.StarExpr); ok {
		e = st.X
	}
	if sel, ok := e.(*ast.SelectorExpr); ok {
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "context" {
			return sel.Sel.Name == "Context"
		}
	}
	return false
}

func isBytes(e ast.Expr) bool {
	if st, ok := e.(*ast.StarExpr); ok {
		e = st.X
	}
	if ar, ok := e.(*ast.ArrayType); ok {
		if id, ok := ar.Elt.(*ast.Ident); ok && id.Name == "byte" {
			return true
		}
	}
	return false
}

// parseDoc 从 doc comment 里抠声明：第一行 = summary；
// when: / input: / example: / summary: 前缀行按字段收。
func parseDoc(m *rpcMethod, warns *[]string) {
	lines := strings.Split(m.doc, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val := splitKv(line)
		switch strings.ToLower(key) {
		case "when":
			m.when = val
		case "input":
			m.input = val
		case "example":
			m.example = val
		case "summary":
			m.summary = val
		default:
			// 第一条无前缀（或不认识的 key）的行当 summary
			if m.summary == "" && key == "" {
				m.summary = line
			}
		}
	}
}

// splitKv 把 "when: xxx" 拆成 ("when", "xxx")；无冒号返回原行。
func splitKv(line string) (string, string) {
	idx := strings.Index(line, ":")
	if idx <= 0 {
		return "", ""
	}
	return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:])
}

// ===== JSON Schema 推导 =====

// objectSchema 从结构体 AST 递归生成 object schema。
// structs 是同包结构体索引，用于展开具名字段；外部包类型一律按 object 兜底。
func objectSchema(st *ast.StructType, structs map[string]*ast.StructType) map[string]any {
	props := map[string]any{}
	var required []string
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			continue // 嵌入字段：不展开，保守处理
		}
		name, omitempty, desc := jsonName(field)
		if name == "-" {
			continue
		}
		props[name] = fieldSchema(field.Type, structs)
		if desc != "" {
			props[name].(map[string]any)["description"] = desc
		}
		if !omitempty {
			required = append(required, name)
		}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		sort.Strings(required)
		schema["required"] = required
	}
	return schema
}

// jsonName 取字段的 json tag；无 omitempty 视为必填；mcp tag 作为描述。
func jsonName(field *ast.Field) (name string, omitempty bool, desc string) {
	name = "?"
	if len(field.Names) > 0 {
		name = strings.ToLower(field.Names[0].Name)
	}
	if field.Tag == nil {
		return name, false, ""
	}
	tag := strings.Trim(field.Tag.Value, "`")
	for _, part := range strings.Split(strings.Trim(tag, "\""), "\"") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "json:"):
			val := strings.TrimPrefix(part, "json:")
			bits := strings.Split(val, ",")
			if bits[0] != "" {
				name = bits[0]
			}
			for _, b := range bits[1:] {
				if b == "omitempty" {
					omitempty = true
				}
			}
		case strings.HasPrefix(part, "mcp:"):
			desc = strings.TrimPrefix(part, "mcp:")
		}
	}
	return name, omitempty, desc
}

// fieldSchema 单个字段类型 → schema 片段。
func fieldSchema(e ast.Expr, structs map[string]*ast.StructType) map[string]any {
	switch t := e.(type) {
	case *ast.StarExpr:
		return fieldSchema(t.X, structs)
	case *ast.Ident:
		return identSchema(t.Name)
	case *ast.SelectorExpr:
		if id, ok := t.X.(*ast.Ident); ok && id.Name == "time" && t.Sel.Name == "Time" {
			return map[string]any{"type": "string", "format": "date-time"}
		}
		return map[string]any{"type": "object", "description": "外部类型 " + exprString(e)}
	case *ast.ArrayType:
		if id, ok := t.Elt.(*ast.Ident); ok && id.Name == "byte" {
			return map[string]any{"type": "string", "contentEncoding": "base64"}
		}
		return map[string]any{"type": "array", "items": fieldSchema(t.Elt, structs)}
	case *ast.MapType:
		return map[string]any{"type": "object", "additionalProperties": fieldSchema(t.Value, structs)}
	case *ast.StructType:
		return objectSchema(t, structs)
	default:
		return map[string]any{"type": "object"}
	}
}

func identSchema(name string) map[string]any {
	var typ string
	switch name {
	case "string":
		typ = "string"
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64":
		typ = "integer"
	case "float32", "float64":
		typ = "number"
	case "bool":
		typ = "boolean"
	default:
		return map[string]any{"type": "object", "description": "类型 " + name}
	}
	return map[string]any{"type": typ}
}

func exprString(e ast.Expr) string {
	raw, _ := json.Marshal(astString(e))
	return string(raw)
}

// astString 极简表达式转字符串（只用于提示信息）。
func astString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return astString(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + astString(t.X)
	}
	return "?"
}

// ===== 产物渲染 =====

func render(pkg string, tools []toolDecl) []byte {
	var b strings.Builder
	b.WriteString("// Code generated by mcpgen; DO NOT EDIT.\n")
	b.WriteString("// 由 rpc-proxy/cmd/mcpgen 从源码注释生成：改注释后重新生成，不要手改本文件。\n\n")
	b.WriteString("package " + pkg + "\n\n")
	b.WriteString("import (\n\t\"context\"\n\t\"encoding/json\"\n)\n\n")

	b.WriteString("// MCPToolDecl 一条能力声明（JSON 字段与 rpc-proxy 侧 registry.ToolDecl 对齐）。\ntype MCPToolDecl struct {\n")
	b.WriteString("\tService     string `json:\"service\"`\n")
	b.WriteString("\tMethod      string `json:\"method\"`\n")
	b.WriteString("\tSummary     string `json:\"summary,omitempty\"`\n")
	b.WriteString("\tWhen        string `json:\"when,omitempty\"`\n")
	b.WriteString("\tInputSchema string `json:\"input_schema,omitempty\"`\n")
	b.WriteString("\tExample     string `json:\"example,omitempty\"`\n}\n\n")

	b.WriteString("// MCPTools 能力清单：mcpgen 从本包 RPC 方法的 doc comment 生成。\nvar MCPTools = []MCPToolDecl{\n")
	for _, t := range tools {
		b.WriteString("\t{\n")
		b.WriteString("\t\tService: " + quote(t.Service) + ",\n")
		b.WriteString("\t\tMethod: " + quote(t.Method) + ",\n")
		if t.Summary != "" {
			b.WriteString("\t\tSummary: " + quote(t.Summary) + ",\n")
		}
		if t.When != "" {
			b.WriteString("\t\tWhen: " + quote(t.When) + ",\n")
		}
		if t.InputSchema != "" {
			b.WriteString("\t\tInputSchema: " + quote(t.InputSchema) + ",\n")
		}
		if t.Example != "" {
			b.WriteString("\t\tExample: " + quote(t.Example) + ",\n")
		}
		b.WriteString("\t},\n")
	}
	b.WriteString("}\n\n")

	b.WriteString(`// MCPMetaService 能力清单服务：Register("meta", &MCPMetaService{}) 挂上后，
// 代理在 Reg 成功后会下行调用 meta.Tools 拉取本清单。
// 没注册这个服务时代理按"未实现"降级（WARN 一条，不影响注册与调用）。
type MCPMetaService struct{}

// Tools 回包结构与 rpc-proxy 侧 registry.ToolsManifest 对齐。
func (MCPMetaService) Tools(ctx context.Context, _ []byte) ([]byte, error) {
	return json.Marshal(map[string]any{
		"schema_version": 1,
		"tools":          MCPTools,
	})
}
`)
	out, err := format.Source([]byte(b.String()))
	if err != nil {
		// format 失败说明生成器模板有 bug，退回未格式化版本，别静默
		fmt.Fprintf(os.Stderr, "mcpgen: WARN 产物 format 失败（%v），按原样写出\n", err)
		return []byte(b.String())
	}
	return out
}

func quote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}
