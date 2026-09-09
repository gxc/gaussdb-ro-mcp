// gaussdb-ro-mcp：基于 GaussDB 官方 Go 驱动的只读 MCP 服务器。
//
// 面向编码代理（Claude Code、OpenCode 等），通过 stdio 传输提供只读数据库工具：
// 连通性测试、schema/表清单、表结构与索引、SELECT 查询。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gaussdb-ro-mcp/internal/config"
	"gaussdb-ro-mcp/internal/db"
	"gaussdb-ro-mcp/internal/tools"
)

var version = "dev" // 由构建时 -ldflags 注入

func main() {
	configPath := flag.String("config", os.Getenv("GAUSSDB_RO_MCP_CONFIG"), "配置文件路径（默认 ./gaussdb-ro-mcp.yaml，或环境变量 GAUSSDB_RO_MCP_CONFIG）")
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()

	logger := log.New(os.Stderr, "[gaussdb-ro-mcp] ", log.LstdFlags)
	if err := start(*configPath, *showVersion, version, logger); err != nil {
		logger.Fatalf("gaussdb-ro-mcp 退出: %v", err)
	}
}

// start 执行完整启动流程；返回错误而非直接退出，便于测试。
func start(configPath string, showVersion bool, ver string, logger *log.Logger) error {
	if showVersion {
		fmt.Println(ver)
		return nil
	}
	if configPath == "" {
		configPath = "gaussdb-ro-mcp.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("配置加载失败: %w", err)
	}
	logger.Printf("已加载配置 %s：%d 个数据源（默认实例: %s），语句超时 %s，max_rows %d",
		configPath, len(cfg.Instances), cfg.DefaultInstance,
		time.Duration(cfg.Server.StatementTimeout), cfg.Server.MaxRows)

	// 优雅退出：SIGINT/SIGTERM 触发 server.Run 返回。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mgr, err := db.NewManager(ctx, cfg)
	if err != nil {
		return fmt.Errorf("初始化数据源失败: %w", err)
	}
	defer mgr.Close()
	logger.Printf("数据源就绪: %s（默认: %s）；会话已强制 READ ONLY", mgr.InstanceNames(), mgr.DefaultInstanceName())

	server := mcp.NewServer(&mcp.Implementation{Name: cfg.Server.Name, Version: ver}, nil)
	tools.Register(server, mgr)

	logger.Printf("MCP 服务器启动（stdio 传输），等待客户端...")
	return server.Run(ctx, &mcp.StdioTransport{})
}
