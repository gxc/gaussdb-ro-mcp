// gaussdb-ro-mcp：基于 GaussDB 官方 Go 驱动的只读 MCP 服务器。
//
// 面向 Coding Agent（Claude Code、OpenCode 等），通过 stdio 传输提供只读数据库工具：
// 连通性测试、schema/表清单、表结构与索引、SELECT 查询。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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

// 项目相关地址（帮助与版本输出使用）。
const (
	issuesURL = "https://github.com/gxc/gaussdb-ro-mcp/issues"
	readmeURL = "https://github.com/gxc/gaussdb-ro-mcp#readme"
	latestURL = "https://github.com/gxc/gaussdb-ro-mcp/releases/latest"
)

func main() {
	fs := registerFlags(flag.CommandLine)
	flag.Usage = func() { printUsage(flag.CommandLine.Output(), version, flag.CommandLine) }
	flag.Parse()

	logger := log.New(os.Stderr, "[gaussdb-ro-mcp] ", log.LstdFlags)
	if err := start(*fs.configPath, *fs.showVersion, version, logger); err != nil {
		if errors.Is(err, context.Canceled) {
			// SIGINT/SIGTERM 触发的正常关闭：以退出码 0 结束，而非记为崩溃。
			logger.Printf("收到退出信号，已正常关闭")
			return
		}
		logger.Fatalf("gaussdb-ro-mcp 退出: %v", err)
	}
}

// progFlags 是命令行参数集合。
type progFlags struct {
	configPath  *string
	showVersion *bool
}

// registerFlags 向 FlagSet 注册命令行参数（main 与测试共用，避免描述漂移）。
func registerFlags(fs *flag.FlagSet) *progFlags {
	return &progFlags{
		configPath:  fs.String("config", os.Getenv("GAUSSDB_RO_MCP_CONFIG"), "配置文件路径（默认 ./gaussdb-ro-mcp.yaml，或环境变量 GAUSSDB_RO_MCP_CONFIG）"),
		showVersion: fs.Bool("version", false, "打印版本号后退出"),
	}
}

// printUsage 输出帮助信息：用法、参数，以及问题反馈与最新版本的获取地址。
func printUsage(w io.Writer, ver string, fs *flag.FlagSet) {
	fs.SetOutput(w)
	fmt.Fprintf(w, "gaussdb-ro-mcp %s — 面向 Coding Agent 的 GaussDB 只读 MCP 服务器（stdio 传输）\n\n", ver)
	fmt.Fprintf(w, "用法：\n  gaussdb-ro-mcp [flags]\n\n参数：\n")
	fs.PrintDefaults()
	fmt.Fprintf(w, "\n详细说明：    %s\n问题反馈：    %s\n获取最新版本： %s\n", readmeURL, issuesURL, latestURL)
}

// start 执行完整启动流程；返回错误而非直接退出，便于测试。
func start(configPath string, showVersion bool, ver string, logger *log.Logger) error {
	if showVersion {
		fmt.Println(ver)
		fmt.Println("获取最新版本：" + latestURL)
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
