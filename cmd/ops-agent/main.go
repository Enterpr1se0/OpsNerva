package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"eino-ops-agent/internal/agent"
	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/httpapi"
	"eino-ops-agent/internal/mcpserver"
	"eino-ops-agent/internal/observability"
	"eino-ops-agent/internal/security"
	"eino-ops-agent/internal/service"
	"eino-ops-agent/internal/sshx"
	"eino-ops-agent/internal/store"
)

const version = "0.2.9"

type application struct {
	config    config.Config
	store     *store.Store
	service   *service.Service
	agent     *agent.Runtime
	startedAt time.Time
}

type serveOptions struct {
	QuickStart     bool
	Desktop        bool
	ConfigPath     string
	ConfigCreated  bool
	MCPHTTPEnabled bool
	WorkspaceRoot  string
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	quickStart := len(args) == 0
	configPath := os.Getenv("OPS_AGENT_CONFIG")
	if len(args) >= 2 && args[0] == "--config" {
		configPath = args[1]
		args = args[2:]
	}
	configCreated := false
	if quickStart {
		args = []string{"serve"}
		if configPath == "" {
			var err error
			configPath, configCreated, err = prepareQuickStart()
			if err != nil {
				return err
			}
		}
	}
	if len(args) == 0 {
		usage()
		return nil
	}
	if args[0] == "version" {
		fmt.Println(version)
		return nil
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := observability.Configure(cfg.Logging); err != nil {
		return fmt.Errorf("configure logging: %w", err)
	}
	slog.InfoContext(ctx, "logging initialized", "component", "server", "level", cfg.Logging.Level, "format", cfg.Logging.Format, "file", cfg.Logging.File)
	app, err := newApplication(ctx, cfg)
	if err != nil {
		return err
	}
	defer app.store.Close()
	defer app.service.CloseMCPServers()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.service.Shutdown(shutdownCtx); err != nil {
			slog.Warn("service shutdown timed out", "component", "server", "error", err)
		}
	}()
	switch args[0] {
	case "serve":
		return serve(ctx, app, serveOptions{
			QuickStart: quickStart, Desktop: quickStart && envBool("OPS_AGENT_DESKTOP"), ConfigPath: configPath,
			ConfigCreated: configCreated, WorkspaceRoot: cfg.WorkspaceDir,
		})
	case "mcp":
		return mcpserver.New(app.service, version).Run(ctx)
	case "host":
		return hostCommand(ctx, app, args[1:])
	case "exec":
		return execCommand(ctx, app, args[1:])
	case "approval":
		return approvalCommand(ctx, app, args[1:])
	case "audit":
		return auditCommand(ctx, app, args[1:])
	case "chat":
		return chatCommand(ctx, app)
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func newApplication(ctx context.Context, cfg config.Config) (*application, error) {
	started := time.Now()
	st, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	encryptor, err := security.NewEncryptor(cfg.MasterKey, cfg.DataDir)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("initialize audit encryption: %w", err)
	}
	transport := sshx.NewNativeSSHTransport(cfg.SSH, cfg.Limits)
	svc := service.New(st, transport, encryptor, security.NewRedactor(), cfg.Limits, cfg)
	if err := svc.InitializeWorkspaces(ctx, cfg.WorkspaceDir); err != nil {
		st.Close()
		return nil, fmt.Errorf("initialize workspaces: %w", err)
	}
	if err := svc.InitializeSkills(); err != nil {
		st.Close()
		return nil, fmt.Errorf("initialize skill registry: %w", err)
	}
	if err := svc.InitializeMCPServers(ctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("initialize MCP servers: %w", err)
	}
	runtime, err := agent.New(ctx, cfg.Model, svc, st)
	if err != nil {
		st.Close()
		return nil, err
	}
	slog.InfoContext(ctx, "application initialized", "component", "server", "duration_ms", time.Since(started).Milliseconds(), "agent_available", runtime.Available())
	return &application{config: cfg, store: st, service: svc, agent: runtime, startedAt: started.UTC()}, nil
}

func prepareQuickStart() (string, bool, error) {
	if appDir := strings.TrimSpace(os.Getenv("OPS_AGENT_HOME")); appDir != "" {
		return prepareQuickStartIn(appDir)
	}
	executable, err := os.Executable()
	if err != nil {
		return "", false, fmt.Errorf("locate executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", false, fmt.Errorf("resolve executable path: %w", err)
	}
	return prepareQuickStartIn(filepath.Dir(executable))
}

func prepareQuickStartIn(appDir string) (string, bool, error) {
	appDir, err := filepath.Abs(appDir)
	if err != nil {
		return "", false, fmt.Errorf("resolve application directory: %w", err)
	}
	if err := os.Chdir(appDir); err != nil {
		return "", false, fmt.Errorf("use executable directory %q: %w", appDir, err)
	}
	configPath := filepath.Join(appDir, config.DefaultFileName)
	created, err := config.EnsureDefaultFile(configPath)
	if err != nil {
		return "", false, err
	}
	return configPath, created, nil
}

func serve(ctx context.Context, app *application, options serveOptions) error {
	if err := app.service.RecoverInterruptedTasks(ctx); err != nil {
		return fmt.Errorf("recover persisted tasks: %w", err)
	}
	listener, err := net.Listen("tcp", app.config.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", app.config.ListenAddress, err)
	}
	defer listener.Close()
	server := &http.Server{
		Addr: app.config.ListenAddress,
		Handler: httpapi.New(app.service, app.agent, httpapi.Options{
			Version:   version,
			StartedAt: app.startedAt,
			Logging:   app.config.Logging,
			Auth:      app.config.Auth,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
	}
	shutdownCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-shutdownCtx.Done()
		slog.Info("server shutdown requested", "component", "server")
		graceCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(graceCtx)
	}()
	address := listener.Addr().String()
	slog.Info("OpsNerva listening", "component", "server", "address", address, "agent_available", app.agent.Available())
	if options.QuickStart {
		url := localWebURL(listener.Addr())
		if options.Desktop {
			settings, settingsErr := app.service.SystemSettings(ctx)
			if settingsErr != nil {
				return fmt.Errorf("read desktop MCP mode: %w", settingsErr)
			}
			options.MCPHTTPEnabled = settings.MCPHTTPEnabled
		}
		printQuickStart(options, url)
		if !options.Desktop {
			if err := openQuickStartBrowser(url); err != nil {
				slog.Debug("could not open browser", "component", "server", "error", err)
			}
		}
	}
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		slog.Info("server stopped", "component", "server")
		return nil
	}
	return err
}

func localWebURL(address net.Addr) string {
	host, port, err := net.SplitHostPort(address.String())
	if err != nil {
		return "http://127.0.0.1:8080"
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func printQuickStart(options serveOptions, url string) {
	if options.Desktop {
		fmt.Println(desktopReadyLine(options, url))
		return
	}
	fmt.Println()
	if options.ConfigCreated {
		fmt.Println("Created configuration:", options.ConfigPath)
	} else {
		fmt.Println("Configuration:", options.ConfigPath)
	}
	fmt.Println("Open:", url)
	fmt.Println("Press Ctrl+C to stop OpsNerva.")
	fmt.Println()
}

func desktopReadyLine(options serveOptions, url string) string {
	payload, _ := json.Marshal(struct {
		URL               string `json:"url"`
		ConfigPath        string `json:"config_path"`
		ConfigurationMade bool   `json:"configuration_created"`
		MCPHTTPEnabled    bool   `json:"mcp_http_enabled"`
		WorkspaceRoot     string `json:"workspace_root"`
	}{
		URL: url, ConfigPath: options.ConfigPath,
		ConfigurationMade: options.ConfigCreated,
		MCPHTTPEnabled:    options.MCPHTTPEnabled,
		WorkspaceRoot:     options.WorkspaceRoot,
	})
	return "OPSNERVA_DESKTOP_READY=" + string(payload)
}

func envBool(name string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(name)))
	return err == nil && value
}

func openQuickStartBrowser(url string) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	command := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url)
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}

func hostCommand(ctx context.Context, app *application, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("host command requires add, list, probe, scan-key, trust, or delete")
	}
	switch args[0] {
	case "list":
		hosts, err := app.service.ListHosts(ctx)
		return printJSON(hosts, err)
	case "add":
		fs := flag.NewFlagSet("host add", flag.ContinueOnError)
		var host domain.HostInput
		var identityPath string
		fs.StringVar(&host.Name, "name", "", "host name")
		fs.StringVar(&host.Address, "address", "", "host address")
		fs.IntVar(&host.Port, "port", 22, "SSH port")
		fs.StringVar(&host.User, "user", "", "SSH user")
		fs.StringVar(&identityPath, "identity", "", "private key file to upload into encrypted storage")
		fs.StringVar(&host.ProxyJumpHostID, "jump-host", "", "registered SSH ProxyJump host ID")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		host.AuthType = "agent"
		host.SudoMode = "none"
		if identityPath != "" {
			privateKey, err := readCLIPrivateKey(identityPath)
			if err != nil {
				return err
			}
			host.AuthType = "key"
			host.PrivateKey = string(privateKey)
		}
		created, err := app.service.SaveHost(ctx, host, "local-cli")
		return printJSON(created, err)
	case "probe", "scan-key", "delete":
		if len(args) < 2 {
			return fmt.Errorf("host ID is required")
		}
		if args[0] == "probe" {
			value, err := app.service.ProbeHost(ctx, args[1], "cli")
			return printJSON(value, err)
		}
		if args[0] == "scan-key" {
			value, err := app.service.ScanHostKey(ctx, args[1])
			return printJSON(value, err)
		}
		return app.service.DeleteHost(ctx, args[1], "local-cli")
	case "trust":
		if len(args) < 3 {
			return fmt.Errorf("usage: host trust HOST_ID FINGERPRINT")
		}
		value, err := app.service.TrustHostKey(ctx, args[1], args[2], "local-cli")
		return printJSON(value, err)
	default:
		return fmt.Errorf("unknown host command %q", args[0])
	}
}

func readCLIPrivateKey(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open SSH private key upload: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("SSH private key upload is not a regular file")
	}
	if info.Size() <= 0 || info.Size() > sshx.MaxPrivateKeyBytes {
		return nil, fmt.Errorf("SSH private key upload has an invalid size")
	}
	data, err := io.ReadAll(io.LimitReader(file, sshx.MaxPrivateKeyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read SSH private key upload: %w", err)
	}
	if err := sshx.ValidatePrivateKey(data); err != nil {
		return nil, fmt.Errorf("invalid SSH private key upload: %w", err)
	}
	return data, nil
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func execCommand(ctx context.Context, app *application, args []string) error {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	var req domain.ExecRequest
	var arguments stringList
	fs.StringVar(&req.HostID, "host", "", "registered host ID or name")
	fs.StringVar(&req.Program, "program", "", "remote program")
	fs.Var(&arguments, "arg", "program argument; repeat as needed")
	fs.StringVar(&req.Script, "script", "", "bash script")
	fs.StringVar(&req.Cwd, "cwd", "", "remote working directory")
	fs.IntVar(&req.TimeoutSeconds, "timeout", 60, "timeout seconds")
	fs.StringVar(&req.Reason, "reason", "local operator request", "operational reason")
	if err := fs.Parse(args); err != nil {
		return err
	}
	req.Args = arguments
	if req.Script != "" {
		req.Mode = domain.ExecScript
	} else {
		req.Mode = domain.ExecProgram
	}
	result, err := app.service.Submit(ctx, req, "local-cli")
	return printJSON(result, err)
}

func approvalCommand(ctx context.Context, app *application, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("approval command requires list, approve, or reject")
	}
	switch args[0] {
	case "list":
		status := "pending"
		if len(args) > 1 {
			status = args[1]
		}
		value, err := app.service.ListApprovals(ctx, status, 100)
		return printJSON(value, err)
	case "approve":
		fs := flag.NewFlagSet("approval approve", flag.ContinueOnError)
		reason := fs.String("reason", "approved by local operator", "approval reason")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("approval ID is required")
		}
		value, err := app.service.Approve(ctx, fs.Arg(0), *reason, "local-cli")
		return printJSON(value, err)
	case "reject":
		if len(args) < 2 {
			return fmt.Errorf("approval ID is required")
		}
		reason := "rejected by local operator"
		if len(args) > 2 {
			reason = strings.Join(args[2:], " ")
		}
		return app.service.Reject(ctx, args[1], reason, "local-cli")
	default:
		return fmt.Errorf("unknown approval command %q", args[0])
	}
}

func auditCommand(ctx context.Context, app *application, args []string) error {
	if len(args) == 0 {
		runs, err := app.service.SearchRuns(ctx, "", "", 50)
		return printJSON(runs, err)
	}
	switch args[0] {
	case "search":
		query := strings.Join(args[1:], " ")
		runs, err := app.service.SearchRuns(ctx, query, "", 100)
		return printJSON(runs, err)
	case "show":
		if len(args) < 2 {
			return fmt.Errorf("run ID is required")
		}
		raw := len(args) > 2 && args[2] == "--raw"
		result, err := app.service.GetRun(ctx, args[1], raw)
		return printJSON(result, err)
	default:
		return fmt.Errorf("unknown audit command %q", args[0])
	}
}

func chatCommand(ctx context.Context, app *application) error {
	if !app.agent.Available() {
		return agent.ErrUnavailable
	}
	scanner := bufio.NewScanner(os.Stdin)
	sessionID := ""
	for {
		fmt.Print("ops> ")
		if !scanner.Scan() {
			return scanner.Err()
		}
		query := strings.TrimSpace(scanner.Text())
		if query == "" {
			continue
		}
		if query == "/exit" || query == "/quit" {
			return nil
		}
		_, err := app.agent.Query(ctx, sessionID, query, func(event agent.Event) {
			if event.SessionID != "" {
				sessionID = event.SessionID
			}
			if event.Type == "message" && event.Content != "" {
				fmt.Print(event.Content)
			}
		})
		fmt.Println()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
	}
}

func printJSON(value any, err error) error {
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func usage() {
	fmt.Println(`OpsNerva ` + version + `

Usage:
  ops-agent                         Create/load config.yaml and start the Web UI
  ops-agent [--config FILE] serve
  ops-agent [--config FILE] chat
  ops-agent [--config FILE] mcp
  ops-agent [--config FILE] host add|list|probe|scan-key|trust|delete
  ops-agent [--config FILE] exec --host ID --program PROGRAM --arg ARG
  ops-agent [--config FILE] approval list|approve|reject
  ops-agent [--config FILE] audit search|show
  ops-agent version`)
}

var _ = strconv.Itoa
