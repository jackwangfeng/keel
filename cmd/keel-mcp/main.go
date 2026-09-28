// Command keel-mcp 是 Keel MCP 服务的本地 stdio 桥（AI 经营 M9，docs/AI经营-M9设计.md §3）。
//
// Keel 的 MCP 服务走 streamable HTTP（/api/v1/mcp）。只支持 stdio 的 harness 可以把它配成本地命令：
//
//	KEEL_MCP_URL=https://<店铺域名>/api/v1/mcp KEEL_AGENT_KEY=kagt_… keel-mcp
//
// 它启动时连上远端、列出工具，原样注册成本地工具；每次调用原样转发。**不含任何业务逻辑**：
// 判权、审计、限流都在服务端。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearer struct {
	key  string
	base http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return b.base.RoundTrip(r)
}

func main() {
	log.SetFlags(0)
	log.SetOutput(os.Stderr) // stdout 是 MCP 的传输通道，日志只能进 stderr
	url, key := strings.TrimSpace(os.Getenv("KEEL_MCP_URL")), strings.TrimSpace(os.Getenv("KEEL_AGENT_KEY"))
	if url == "" || key == "" {
		log.Fatal("keel-mcp：要设 KEEL_MCP_URL（如 https://eshop.zzss.fun/api/v1/mcp）与 KEEL_AGENT_KEY（kagt_ 密钥）")
	}
	ctx := context.Background()
	remote, err := mcp.NewClient(&mcp.Implementation{Name: "keel-mcp-bridge", Version: "1"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: url, DisableStandaloneSSE: true,
			HTTPClient: &http.Client{Transport: bearer{key: key, base: http.DefaultTransport}}}, nil)
	if err != nil {
		log.Fatalf("keel-mcp：连不上 %s：%v", url, err)
	}
	defer remote.Close()

	info := remote.InitializeResult()
	srv := mcp.NewServer(&mcp.Implementation{Name: "keel", Version: info.ServerInfo.Version},
		&mcp.ServerOptions{Instructions: info.Instructions})
	n := 0
	for tool, err := range remote.Tools(ctx, nil) {
		if err != nil {
			log.Fatalf("keel-mcp：列工具失败：%v", err)
		}
		name := tool.Name
		srv.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args any
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return nil, fmt.Errorf("参数不是合法的 JSON：%w", err)
				}
			}
			return remote.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		})
		n++
	}
	log.Printf("keel-mcp：已连上 %s，转发 %d 个工具", url, n)
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
