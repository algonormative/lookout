package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/algonormative/lookout/internal/api"
	"github.com/algonormative/lookout/internal/cli"
	"github.com/algonormative/lookout/internal/client"
	"github.com/algonormative/lookout/internal/config"
	mcpsrv "github.com/algonormative/lookout/internal/mcp"
	"github.com/algonormative/lookout/internal/otlp"
	"github.com/algonormative/lookout/internal/store"
	"github.com/algonormative/lookout/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "query":
		cmdQuery(os.Args[2:])
	case "mcp":
		cmdMCP(os.Args[2:])
	case "mcp-serve":
		cmdMCPServe(os.Args[2:])
	case "dash":
		cmdDash(os.Args[2:])
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`lookout — OTEL trace collector for AI workflows

Usage:
  lookout <command> [flags]

Commands:
  serve     Start the collector daemon
  query     Query spans, sessions, metrics, stats, anomalies
  mcp       Start MCP server (stdio)
  mcp-serve Start MCP server (HTTP)
  dash      Launch TUI dashboard

Run "lookout <command> --help" for details.`)
}

func cmdServe(args []string) {
	cfg := config.Default()
	cfg.ApplyEnv()

	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.StringVar(&cfg.GRPCAddr, "grpc-addr", cfg.GRPCAddr, "OTLP gRPC address")
	fs.StringVar(&cfg.HTTPAddr, "http-addr", cfg.HTTPAddr, "OTLP HTTP address")
	fs.StringVar(&cfg.APIAddr, "api-addr", cfg.APIAddr, "Analytics API address")
	fs.StringVar(&cfg.DBPath, "db-path", cfg.DBPath, "SQLite database path")
	fs.IntVar(&cfg.RingSize, "ring-size", cfg.RingSize, "Ring buffer capacity")
	mcpAddr := fs.String("mcp-addr", ":4321", "MCP HTTP server address (empty to disable)")
	fs.IntVar(&cfg.RetentionDays, "retention-days", cfg.RetentionDays, "Retention period in days")
	fs.Parse(args)

	// Ensure DB directory exists
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0755); err != nil {
		log.Fatalf("create db dir: %v", err)
	}

	// Open store
	sqlStore, err := store.NewSQLiteStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer sqlStore.Close()

	// Create ring buffer
	ring := store.NewRing(cfg.RingSize)

	// Create API server
	apiServer := api.NewServer(cfg.APIAddr, sqlStore, ring)

	// Create OTLP receivers
	otlpHTTP := otlp.NewHTTPReceiver(cfg.HTTPAddr, sqlStore, ring, apiServer.BroadcastSpans)
	otlpGRPC := otlp.NewGRPCReceiver(cfg.GRPCAddr, sqlStore, ring, apiServer.BroadcastSpans)

	// Start servers
	if err := otlpGRPC.Start(); err != nil {
		log.Fatalf("start otlp grpc: %v", err)
	}
	if err := otlpHTTP.Start(); err != nil {
		log.Fatalf("start otlp http: %v", err)
	}
	if err := apiServer.Start(); err != nil {
		log.Fatalf("start api: %v", err)
	}

	// Start MCP HTTP server if address is set
	if *mcpAddr != "" {
		mcpSrv := mcpsrv.NewServer(sqlStore)
		httpMCP := server.NewStreamableHTTPServer(mcpSrv, server.WithStateLess(true))
		go func() {
			log.Printf("MCP HTTP server listening on %s", *mcpAddr)
			if err := httpMCP.Start(*mcpAddr); err != nil {
				log.Printf("mcp-serve error: %v", err)
			}
		}()
	}

	log.Printf("lookout running — OTLP gRPC %s, OTLP HTTP %s, API %s, DB %s", cfg.GRPCAddr, cfg.HTTPAddr, cfg.APIAddr, cfg.DBPath)

	// Start retention cleanup loop
	go retentionLoop(sqlStore, cfg.RetentionDays)

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	otlpGRPC.Stop()
	otlpHTTP.Stop(ctx)
	apiServer.Stop(ctx)
}


func cmdMCPServe(args []string) {
	cfg := config.Default()
	cfg.ApplyEnv()

	fs := flag.NewFlagSet("mcp-serve", flag.ExitOnError)
	addr := fs.String("addr", ":4321", "MCP HTTP server address")
	fs.StringVar(&cfg.DBPath, "db-path", cfg.DBPath, "SQLite database path")
	fs.Parse(args)

	sqlStore, err := store.NewSQLiteStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer sqlStore.Close()

	mcpSrv := mcpsrv.NewServer(sqlStore)
	httpSrv := server.NewStreamableHTTPServer(mcpSrv, server.WithStateLess(true))
	log.Printf("MCP HTTP server listening on %s", *addr)
	if err := httpSrv.Start(*addr); err != nil {
		log.Fatalf("mcp-serve: %v", err)
	}
}

func retentionLoop(s *store.SQLiteStore, retentionDays int) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		n, err := s.Cleanup(context.Background(), retentionDays)
		if err != nil {
			log.Printf("cleanup error: %v", err)
		} else if n > 0 {
			log.Printf("cleanup: deleted %d old spans", n)
		}
	}
}

func cmdDash(args []string) {
	cfg := config.Default()
	cfg.ApplyEnv()

	fs := flag.NewFlagSet("dash", flag.ExitOnError)
	fs.StringVar(&cfg.DBPath, "db-path", cfg.DBPath, "SQLite database path")
	fs.Parse(args)

	sqlStore, err := store.NewSQLiteStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer sqlStore.Close()

	p := tea.NewProgram(tui.New(sqlStore), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		log.Fatalf("tui: %v", err)
	}
}

func cmdMCP(args []string) {
	cfg := config.Default()
	cfg.ApplyEnv()

	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	fs.StringVar(&cfg.DBPath, "db-path", cfg.DBPath, "SQLite database path")
	fs.Parse(args)

	sqlStore, err := store.NewSQLiteStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer sqlStore.Close()

	srv := mcpsrv.NewServer(sqlStore)
	if err := server.ServeStdio(srv); err != nil {
		log.Fatalf("mcp server: %v", err)
	}
}

func cmdQuery(args []string) {
	if len(args) == 0 {
		fmt.Println(`Usage: lookout query <subcommand> [flags]

Subcommands:
  traces      Query trace spans
  sessions    Query agent sessions
  metrics     Query metric rollups
  stats       Query aggregate statistics
  anomalies   Query anomalous spans`)
		os.Exit(1)
	}

	cfg := config.Default()
	cfg.ApplyEnv()

	subcmd := args[0]
	subArgs := args[1:]

	// Use API client if LOOKOUT_API_URL is set, otherwise open local SQLite
	var s store.Store
	if cfg.APIURL != "" {
		s = client.New(cfg.APIURL)
	} else {
		sqlStore, err := store.NewSQLiteStore(cfg.DBPath)
		if err != nil {
			log.Fatalf("open store: %v", err)
		}
		defer sqlStore.Close()
		s = sqlStore
	}

	ctx := context.Background()

	switch subcmd {
	case "traces":
		var a cli.QueryTracesArgs
		fs := flag.NewFlagSet("query traces", flag.ExitOnError)
		fs.StringVar(&a.TraceID, "trace-id", "", "Filter by trace ID")
		fs.StringVar(&a.Agent, "agent", "", "Filter by agent name")
		fs.StringVar(&a.Model, "model", "", "Filter by model")
		fs.StringVar(&a.Since, "since", "", "Time range start (e.g., 1h, 24h)")
		fs.StringVar(&a.Until, "until", "", "Time range end")
		fs.StringVar(&a.DurationGt, "duration-gt", "", "Min duration (e.g., 5s, 1m)")
		fs.StringVar(&a.Status, "status", "", "Filter by status (ok, error, unset)")
		fs.Float64Var(&a.CostGt, "cost-gt", 0, "Min cost in USD")
		fs.StringVar(&a.SortBy, "sort-by", "time", "Sort field (time, duration, cost, tokens)")
		fs.IntVar(&a.Limit, "limit", 20, "Max results")
		fs.StringVar(&a.Format, "format", "table", "Output format: table|json|csv")
		fs.Parse(subArgs)
		if err := cli.QueryTraces(ctx, s, a); err != nil {
			log.Fatal(err)
		}

	case "sessions":
		var a cli.QuerySessionsArgs
		fs := flag.NewFlagSet("query sessions", flag.ExitOnError)
		fs.StringVar(&a.Agent, "agent", "", "Filter by agent name")
		fs.StringVar(&a.Since, "since", "", "Time range")
		fs.IntVar(&a.Limit, "limit", 20, "Max results")
		fs.StringVar(&a.Format, "format", "table", "Output format: table|json|csv")
		fs.Parse(subArgs)
		if err := cli.QuerySessions(ctx, s, a); err != nil {
			log.Fatal(err)
		}

	case "metrics":
		var a cli.QueryMetricsArgs
		fs := flag.NewFlagSet("query metrics", flag.ExitOnError)
		fs.StringVar(&a.Name, "name", "", "Metric name (required)")
		fs.StringVar(&a.Since, "since", "1h", "Time range (default 1h)")
		fs.StringVar(&a.Bucket, "bucket", "1m", "Bucket width: 1m|1h|1d")
		fs.StringVar(&a.Labels, "labels", "", "Label filter (key:val,key2:val2)")
		fs.StringVar(&a.Format, "format", "table", "Output format: table|json|csv")
		fs.Parse(subArgs)
		if a.Name == "" {
			fmt.Fprintln(os.Stderr, "--name is required for metrics query")
			os.Exit(1)
		}
		if err := cli.QueryMetrics(ctx, s, a); err != nil {
			log.Fatal(err)
		}

	case "stats":
		var a cli.QueryStatsArgs
		fs := flag.NewFlagSet("query stats", flag.ExitOnError)
		fs.StringVar(&a.Since, "since", "", "Time range")
		fs.StringVar(&a.GroupBy, "group-by", "", "Group by: model|agent")
		fs.StringVar(&a.Format, "format", "table", "Output format: table|json|csv")
		fs.Parse(subArgs)
		if err := cli.QueryStats(ctx, s, a); err != nil {
			log.Fatal(err)
		}

	case "anomalies":
		var a cli.QueryAnomaliesArgs
		fs := flag.NewFlagSet("query anomalies", flag.ExitOnError)
		fs.StringVar(&a.Since, "since", "", "Time range")
		fs.StringVar(&a.Agent, "agent", "", "Filter by agent")
		fs.IntVar(&a.Limit, "limit", 20, "Max results")
		fs.StringVar(&a.Format, "format", "table", "Output format: table|json|csv")
		fs.Parse(subArgs)
		if err := cli.QueryAnomalies(ctx, s, a); err != nil {
			log.Fatal(err)
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown query subcommand: %s\n", subcmd)
		os.Exit(1)
	}
}
