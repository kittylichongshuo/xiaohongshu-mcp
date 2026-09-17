package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

func TestSearchDiagnosticsHandlerValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.jsonl")
	t.Setenv("XHS_SEARCH_DIAGNOSTICS_PATH", path)
	// Missing keyword exits before service/browser creation, exercising the real
	// handler safely without contacting MCP or any external site.
	result := (&AppServer{}).handleSearchFeeds(context.Background(), SearchFeedsArgs{})
	if !result.IsError || result.Content[0].Text != "搜索Feeds失败: 缺少关键词参数" {
		t.Fatal("handler semantics changed")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"search_handler_start", "response_ready", "handler_return"} {
		if !strings.Contains(string(data), stage) {
			t.Fatal("missing", stage)
		}
	}
	if strings.Contains(string(data), "browser_context_created") {
		t.Fatal("browser created")
	}
}

func TestSearchDiagnosticsExistingRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.jsonl")
	t.Setenv("XHS_SEARCH_DIAGNOSTICS_PATH", path)
	previous := logrus.StandardLogger().Out
	logrus.SetOutput(io.Discard)
	defer logrus.SetOutput(previous)
	sentinel := errors.New("synthetic sentinel")
	wrapped := withPanicRecovery("search_feeds", func(ctx context.Context, _ *mcp.CallToolRequest, _ SearchFeedsArgs) (*mcp.CallToolResult, any, error) {
		ctx, d := xiaohongshu.StartSearchDiagnostics(ctx)
		defer d.Close()
		defer d.Finish(ctx, false, false)
		d.Step(ctx, "stable_wait", func() { panic(sentinel) })
		return nil, nil, nil
	})
	result, _, err := wrapped(context.Background(), nil, SearchFeedsArgs{})
	if err != nil || !result.IsError {
		t.Fatal("existing recovery changed")
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, sentinel.Error()) {
		t.Fatal("panic payload changed before original recovery")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), sentinel.Error()) || !strings.Contains(string(data), "panic_unwind") {
		t.Fatal("unsafe or missing diagnostic")
	}
}
