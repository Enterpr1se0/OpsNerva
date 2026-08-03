package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/observability"
	"eino-ops-agent/internal/proxyx"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	einojsonschema "github.com/eino-contrib/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpConnectTimeout = 20 * time.Second
	mcpCallTimeout    = 90 * time.Second
	mcpMaxSchemaBytes = 256 << 10
	mcpMaxTools       = 128
)

var (
	mcpEnvNameRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	mcpToolNameRE = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
)

type mcpSecrets struct {
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	OAuth   *mcpOAuthSession  `json:"oauth,omitempty"`
}

type mcpRuntimeState struct {
	status      string
	lastError   string
	connectedAt *time.Time
	session     *mcp.ClientSession
	tools       []mcpResolvedTool
}

type mcpResolvedTool struct {
	Name        string
	ExposedName string
	Description string
	Schema      *einojsonschema.Schema
}

type mcpDynamicTool struct {
	service  *Service
	serverID string
	resolved mcpResolvedTool
}

func (t *mcpDynamicTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name:        t.resolved.ExposedName,
		Desc:        t.resolved.Description,
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(t.resolved.Schema),
	}, nil
}

func (t *mcpDynamicTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var arguments any = map[string]any{}
	if strings.TrimSpace(argumentsInJSON) != "" {
		if err := json.Unmarshal([]byte(argumentsInJSON), &arguments); err != nil {
			return "", fmt.Errorf("invalid MCP tool arguments: %w", err)
		}
	}
	return t.service.callMCPTool(ctx, t.serverID, t.resolved.Name, arguments)
}

func (s *Service) InitializeMCPServers(ctx context.Context) error {
	servers, err := s.store.ListMCPServers(ctx)
	if err != nil {
		return err
	}
	for _, server := range servers {
		if !server.Enabled {
			s.setMCPRuntime(server.ID, &mcpRuntimeState{status: "disabled"})
			continue
		}
		if err := s.ReconnectMCPServer(ctx, server.ID); err != nil {
			observability.FromContext(ctx).WarnContext(ctx, "MCP server initialization failed", "component", "mcp_client", "server_id", server.ID, "server_name", server.Name, "error", err)
		}
	}
	return nil
}

func (s *Service) CloseMCPServers() {
	s.cancelAllMCPOAuthFlows()
	s.mcpMu.Lock()
	states := s.mcpRuntime
	s.mcpRuntime = make(map[string]*mcpRuntimeState)
	s.mcpMu.Unlock()
	for _, state := range states {
		if state != nil && state.session != nil {
			_ = state.session.Close()
		}
	}
}

func (s *Service) SaveMCPServer(ctx context.Context, input domain.MCPServerInput, actor string) (domain.MCPServer, error) {
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Command = strings.TrimSpace(input.Command)
	input.Cwd = strings.TrimSpace(input.Cwd)
	input.URL = strings.TrimSpace(input.URL)
	if err := validateMCPInput(input); err != nil {
		return domain.MCPServer{}, err
	}
	if input.ID != "" {
		s.cancelMCPOAuthFlow(input.ID)
	}

	s.mcpSecretsMu.Lock()
	saved, err := s.storeMCPServerConfig(ctx, input)
	s.mcpSecretsMu.Unlock()
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
			return domain.MCPServer{}, fmt.Errorf("MCP server name already exists")
		}
		return domain.MCPServer{}, err
	}
	if saved.Enabled {
		_ = s.ReconnectMCPServer(ctx, saved.ID)
	} else {
		s.disconnectMCPServer(saved.ID, "disabled")
	}
	s.audit(ctx, "", "mcp_server_saved", actor, map[string]any{"server_id": saved.ID, "name": saved.Name, "transport": saved.Transport, "enabled": saved.Enabled})
	return s.GetMCPServer(ctx, saved.ID)
}

func (s *Service) storeMCPServerConfig(ctx context.Context, input domain.MCPServerInput) (domain.MCPServer, error) {
	server := domain.MCPServer{
		ID: input.ID, Name: input.Name, Transport: input.Transport, Command: input.Command,
		Args: append([]string(nil), input.Args...), Cwd: input.Cwd, URL: input.URL, Enabled: input.Enabled,
	}
	secrets := mcpSecrets{Env: map[string]string{}, Headers: map[string]string{}}
	if input.ID != "" {
		existing, err := s.store.GetMCPServer(ctx, input.ID)
		if err != nil {
			return domain.MCPServer{}, err
		}
		server.CreatedAt = existing.CreatedAt
		server.SecretsCipher = existing.SecretsCipher
		secrets, err = s.decryptMCPSecrets(existing.SecretsCipher)
		if err != nil {
			return domain.MCPServer{}, fmt.Errorf("decrypt existing MCP secrets: %w", err)
		}
		if existing.Transport != input.Transport || existing.URL != input.URL {
			secrets.OAuth = nil
		}
	}
	if input.Env != nil {
		secrets.Env = cloneStringMap(input.Env)
	}
	if input.Headers != nil {
		secrets.Headers = cloneStringMap(input.Headers)
	}
	if err := validateMCPSecrets(secrets); err != nil {
		return domain.MCPServer{}, err
	}
	server.EnvKeys = sortedMapKeys(secrets.Env)
	server.HeaderKeys = sortedMapKeys(secrets.Headers)
	payload, err := json.Marshal(secrets)
	if err != nil {
		return domain.MCPServer{}, err
	}
	server.SecretsCipher, err = s.encryptor.Encrypt(payload)
	if err != nil {
		return domain.MCPServer{}, err
	}
	saved, err := s.store.UpsertMCPServer(ctx, server)
	return saved, err
}

func (s *Service) ListMCPServers(ctx context.Context) ([]domain.MCPServer, error) {
	servers, err := s.store.ListMCPServers(ctx)
	if err != nil {
		return nil, err
	}
	for index := range servers {
		servers[index] = s.decorateMCPServer(servers[index])
	}
	return servers, nil
}

func (s *Service) GetMCPServer(ctx context.Context, id string) (domain.MCPServer, error) {
	server, err := s.store.GetMCPServer(ctx, id)
	if err != nil {
		return domain.MCPServer{}, err
	}
	return s.decorateMCPServer(server), nil
}

func (s *Service) SetMCPServerEnabled(ctx context.Context, id string, enabled bool, actor string) (domain.MCPServer, error) {
	server, err := s.store.GetMCPServer(ctx, id)
	if err != nil {
		return domain.MCPServer{}, err
	}
	if err := s.store.SetMCPServerEnabled(ctx, id, enabled); err != nil {
		return domain.MCPServer{}, err
	}
	if enabled {
		_ = s.ReconnectMCPServer(ctx, id)
	} else {
		s.cancelMCPOAuthFlow(id)
		s.disconnectMCPServer(id, "disabled")
	}
	eventType := "mcp_server_disabled"
	if enabled {
		eventType = "mcp_server_enabled"
	}
	s.audit(ctx, "", eventType, actor, map[string]any{"server_id": id, "name": server.Name})
	return s.GetMCPServer(ctx, id)
}

func (s *Service) DeleteMCPServer(ctx context.Context, id, actor string) error {
	server, err := s.store.GetMCPServer(ctx, id)
	if err != nil {
		return err
	}
	s.cancelMCPOAuthFlow(id)
	s.disconnectMCPServer(id, "disabled")
	if err := s.store.DeleteMCPServer(ctx, id); err != nil {
		return err
	}
	s.mcpMu.Lock()
	delete(s.mcpRuntime, id)
	s.mcpMu.Unlock()
	s.audit(ctx, "", "mcp_server_deleted", actor, map[string]any{"server_id": id, "name": server.Name})
	return nil
}

func (s *Service) ReconnectMCPServer(ctx context.Context, id string) error {
	server, err := s.store.GetMCPServer(ctx, id)
	if err != nil {
		return err
	}
	if !server.Enabled {
		return fmt.Errorf("MCP server is disabled")
	}
	s.mcpMu.Lock()
	previous := s.mcpRuntime[id]
	s.mcpRuntime[id] = &mcpRuntimeState{status: "connecting"}
	s.mcpMu.Unlock()
	if previous != nil && previous.session != nil {
		_ = previous.session.Close()
	}

	started := time.Now()
	connectCtx, cancel := context.WithTimeout(ctx, mcpConnectTimeout)
	defer cancel()
	session, tools, err := s.connectMCPServer(connectCtx, server)
	if err != nil {
		s.setMCPRuntime(id, &mcpRuntimeState{status: "error", lastError: err.Error()})
		observability.FromContext(ctx).ErrorContext(ctx, "MCP server connection failed", "component", "mcp_client", "server_id", id, "server_name", server.Name, "transport", server.Transport, "duration_ms", time.Since(started).Milliseconds(), "error", err)
		return err
	}
	now := time.Now().UTC()
	s.setMCPRuntime(id, &mcpRuntimeState{status: "ready", connectedAt: &now, session: session, tools: tools})
	observability.FromContext(ctx).InfoContext(ctx, "MCP server ready", "component", "mcp_client", "server_id", id, "server_name", server.Name, "transport", server.Transport, "tool_count", len(tools), "duration_ms", time.Since(started).Milliseconds())
	return nil
}

func (s *Service) TestMCPServer(ctx context.Context, id string) (domain.MCPTestResult, error) {
	server, err := s.store.GetMCPServer(ctx, id)
	if err != nil {
		return domain.MCPTestResult{}, err
	}
	started := time.Now()
	testCtx, cancel := context.WithTimeout(ctx, mcpConnectTimeout)
	defer cancel()
	session, tools, err := s.connectMCPServer(testCtx, server)
	if err != nil {
		return domain.MCPTestResult{}, err
	}
	defer session.Close()
	return domain.MCPTestResult{OK: true, LatencyMS: time.Since(started).Milliseconds(), ToolCount: len(tools), Tools: publicMCPTools(tools)}, nil
}

func (s *Service) MCPTools() []tool.BaseTool {
	s.mcpMu.RLock()
	defer s.mcpMu.RUnlock()
	result := make([]tool.BaseTool, 0)
	for serverID, state := range s.mcpRuntime {
		if state == nil || state.status != "ready" || state.session == nil {
			continue
		}
		for _, resolved := range state.tools {
			result = append(result, &mcpDynamicTool{service: s, serverID: serverID, resolved: resolved})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		left, _ := result[i].Info(context.Background())
		right, _ := result[j].Info(context.Background())
		return left.Name < right.Name
	})
	return result
}

func (s *Service) callMCPTool(ctx context.Context, serverID, toolName string, arguments any) (string, error) {
	started := time.Now()
	s.mcpMu.RLock()
	state := s.mcpRuntime[serverID]
	if state == nil || state.status != "ready" || state.session == nil {
		s.mcpMu.RUnlock()
		return "", fmt.Errorf("MCP server is not ready")
	}
	callCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, mcpCallTimeout)
		defer cancel()
	}
	result, err := state.session.CallTool(callCtx, &mcp.CallToolParams{Name: toolName, Arguments: arguments})
	s.mcpMu.RUnlock()
	status := "completed"
	if err != nil {
		status = "failed"
	} else if result == nil {
		status = "failed"
		err = fmt.Errorf("MCP server returned an empty tool result")
	} else if result.IsError {
		status = "tool_error"
	}
	s.audit(ctx, "", "mcp_tool_called", "eino-agent", map[string]any{
		"server_id": serverID, "tool_name": toolName, "status": status, "duration_ms": time.Since(started).Milliseconds(), "session_id": SessionIDFromContext(ctx),
	})
	if err != nil {
		return "", err
	}
	envelope := map[string]any{
		"tool_version":         "1.1",
		"ok":                   !result.IsError,
		"status":               "completed",
		"code":                 "completed",
		"content_is_untrusted": true,
		"result":               result,
	}
	if result.IsError {
		envelope["status"] = "failed"
		envelope["code"] = "provider_failed"
		envelope["message"] = "the external MCP function tool returned an error"
		envelope["next_action"] = "inspect the returned error and external state; do not repeat the same call unchanged"
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func (s *Service) connectMCPServer(ctx context.Context, server domain.MCPServer) (*mcp.ClientSession, []mcpResolvedTool, error) {
	secrets, err := s.decryptMCPSecrets(server.SecretsCipher)
	if err != nil {
		return nil, nil, fmt.Errorf("decrypt MCP secrets: %w", err)
	}
	var oauthHandler auth.OAuthHandler
	if server.Transport == domain.MCPTransportStreamableHTTP && secrets.OAuth != nil {
		oauthClient := &http.Client{Timeout: mcpCallTimeout, Transport: http.DefaultTransport}
		oauthHandler, err = s.restoredMCPOAuthHandler(server.ID, secrets.OAuth, oauthClient)
		if err != nil {
			return nil, nil, fmt.Errorf("restore MCP OAuth session: %w", err)
		}
	}
	return s.connectMCPServerWithSecrets(ctx, server, secrets, oauthHandler)
}

func (s *Service) connectMCPServerWithSecrets(ctx context.Context, server domain.MCPServer, secrets mcpSecrets, oauthHandler auth.OAuthHandler) (*mcp.ClientSession, []mcpResolvedTool, error) {
	var transport mcp.Transport
	switch server.Transport {
	case domain.MCPTransportStdio:
		command := exec.Command(server.Command, server.Args...)
		command.Dir = server.Cwd
		command.Env = append(os.Environ(), mapAsEnvironment(secrets.Env)...)
		transport = &mcp.CommandTransport{Command: command}
	case domain.MCPTransportStreamableHTTP:
		client := mcpResourceHTTPClient(secrets.Headers, oauthHandler != nil)
		return s.connectMCPServerWithTransport(ctx, server, client, oauthHandler)
	default:
		return nil, nil, fmt.Errorf("unsupported MCP transport %q", server.Transport)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "opsnerva", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, nil, err
	}
	resolved, err := listMCPTools(ctx, session, server.ID, server.Name)
	if err != nil {
		_ = session.Close()
		return nil, nil, err
	}
	return session, resolved, nil
}

func (s *Service) connectMCPServerWithTransport(ctx context.Context, server domain.MCPServer, client *http.Client, oauthHandler auth.OAuthHandler) (*mcp.ClientSession, []mcpResolvedTool, error) {
	transport := &mcp.StreamableClientTransport{
		Endpoint: server.URL, HTTPClient: client, OAuthHandler: oauthHandler,
		MaxRetries: 1, DisableStandaloneSSE: true,
	}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "opsnerva", Version: "0.1.0"}, nil)
	session, err := mcpClient.Connect(ctx, transport, nil)
	if err != nil {
		return nil, nil, err
	}
	resolved, err := listMCPTools(ctx, session, server.ID, server.Name)
	if err != nil {
		_ = session.Close()
		return nil, nil, err
	}
	return session, resolved, nil
}

func mcpResourceHTTPClient(headers map[string]string, oauthEnabled bool) *http.Client {
	fixed := cloneStringMap(headers)
	if oauthEnabled {
		for name := range fixed {
			if strings.EqualFold(name, "Authorization") {
				delete(fixed, name)
			}
		}
	}
	return &http.Client{
		Timeout:   mcpCallTimeout,
		Transport: proxyx.HeaderRewriteTransport{Base: http.DefaultTransport, Headers: fixed},
	}
}

func listMCPTools(ctx context.Context, session *mcp.ClientSession, serverID, serverName string) ([]mcpResolvedTool, error) {
	result := make([]mcpResolvedTool, 0)
	cursor := ""
	seen := make(map[string]struct{})
toolPages:
	for page := 0; page < 100; page++ {
		response, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, candidate := range response.Tools {
			if len(result) >= mcpMaxTools {
				break toolPages
			}
			if candidate == nil || strings.TrimSpace(candidate.Name) == "" {
				continue
			}
			exposed := exposedMCPToolName(serverID, candidate.Name)
			if _, exists := seen[exposed]; exists {
				digest := sha256.Sum256([]byte(candidate.Name))
				exposed = truncateToolName(exposed, 55) + "_" + hex.EncodeToString(digest[:4])
			}
			seen[exposed] = struct{}{}
			inputSchema := &einojsonschema.Schema{Type: "object"}
			if candidate.InputSchema != nil {
				if encoded, marshalErr := json.Marshal(candidate.InputSchema); marshalErr == nil && len(encoded) <= mcpMaxSchemaBytes {
					var decoded einojsonschema.Schema
					if json.Unmarshal(encoded, &decoded) == nil {
						inputSchema = &decoded
					}
				}
			}
			description := strings.TrimSpace(candidate.Description)
			if description == "" {
				description = "External MCP tool " + candidate.Name
			}
			if len(description) > 2000 {
				description = strings.ToValidUTF8(description[:2000], "�") + "…"
			}
			description = fmt.Sprintf("MCP server %q: %s", serverName, description)
			result = append(result, mcpResolvedTool{Name: candidate.Name, ExposedName: exposed, Description: description, Schema: inputSchema})
		}
		if response.NextCursor == "" {
			break
		}
		cursor = response.NextCursor
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ExposedName < result[j].ExposedName })
	return result, nil
}

func (s *Service) decorateMCPServer(server domain.MCPServer) domain.MCPServer {
	if secrets, err := s.decryptMCPSecrets(server.SecretsCipher); err == nil && secrets.OAuth != nil {
		server.OAuthConfigured = true
		if !secrets.OAuth.Expiry.IsZero() {
			expiresAt := secrets.OAuth.Expiry
			server.OAuthExpiresAt = &expiresAt
		}
	}
	s.mcpMu.RLock()
	state := s.mcpRuntime[server.ID]
	if state != nil {
		server.Status = state.status
		server.LastError = state.lastError
		server.ConnectedAt = state.connectedAt
		server.Tools = publicMCPTools(state.tools)
		server.ToolCount = len(server.Tools)
	}
	s.mcpMu.RUnlock()
	if server.Status == "" {
		if server.Enabled {
			server.Status = "disconnected"
		} else {
			server.Status = "disabled"
		}
	}
	server.SecretsCipher = ""
	return server
}

func (s *Service) setMCPRuntime(id string, replacement *mcpRuntimeState) {
	s.mcpMu.Lock()
	previous := s.mcpRuntime[id]
	s.mcpRuntime[id] = replacement
	s.mcpMu.Unlock()
	if previous != nil && previous.session != nil && previous.session != replacement.session {
		_ = previous.session.Close()
	}
}

func (s *Service) disconnectMCPServer(id, status string) {
	s.setMCPRuntime(id, &mcpRuntimeState{status: status})
}

func (s *Service) decryptMCPSecrets(ciphertext string) (mcpSecrets, error) {
	result := mcpSecrets{Env: map[string]string{}, Headers: map[string]string{}}
	plain, err := s.encryptor.Decrypt(ciphertext)
	if err != nil {
		return result, err
	}
	if len(plain) == 0 {
		return result, nil
	}
	if err := json.Unmarshal(plain, &result); err != nil {
		return result, err
	}
	if result.Env == nil {
		result.Env = map[string]string{}
	}
	if result.Headers == nil {
		result.Headers = map[string]string{}
	}
	return result, nil
}

func validateMCPInput(input domain.MCPServerInput) error {
	if input.Name == "" || len(input.Name) > 80 {
		return fmt.Errorf("MCP server name must contain 1-80 characters")
	}
	if len(input.Args) > 64 {
		return fmt.Errorf("MCP command supports at most 64 arguments")
	}
	for _, argument := range input.Args {
		if strings.ContainsRune(argument, 0) || len(argument) > 4096 {
			return fmt.Errorf("MCP command contains an invalid argument")
		}
	}
	switch input.Transport {
	case domain.MCPTransportStdio:
		if input.Command == "" || strings.ContainsRune(input.Command, 0) {
			return fmt.Errorf("command is required for stdio MCP servers")
		}
		if input.Cwd != "" && !filepath.IsAbs(input.Cwd) {
			return fmt.Errorf("cwd must be an absolute path")
		}
	case domain.MCPTransportStreamableHTTP:
		parsed, err := url.Parse(input.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("url must be an absolute http or https URL")
		}
		if parsed.User != nil {
			return fmt.Errorf("URL credentials are not supported; use an HTTP header")
		}
	default:
		return fmt.Errorf("transport must be stdio or streamable_http")
	}
	return nil
}

func validateMCPSecrets(secrets mcpSecrets) error {
	if len(secrets.Env) > 64 || len(secrets.Headers) > 64 {
		return fmt.Errorf("MCP secrets support at most 64 environment variables and 64 headers")
	}
	for name, value := range secrets.Env {
		if !mcpEnvNameRE.MatchString(name) || strings.ContainsRune(value, 0) || len(value) > 32<<10 {
			return fmt.Errorf("invalid MCP environment variable %q", name)
		}
	}
	for name, value := range secrets.Headers {
		if strings.TrimSpace(name) == "" || http.CanonicalHeaderKey(name) == "" || strings.ContainsAny(name+value, "\r\n") || len(value) > 32<<10 {
			return fmt.Errorf("invalid MCP HTTP header %q", name)
		}
		switch strings.ToLower(name) {
		case "host", "content-length", "content-type", "accept", "mcp-session-id", "last-event-id":
			return fmt.Errorf("MCP HTTP header %q is managed by the protocol", name)
		}
	}
	return nil
}

func exposedMCPToolName(serverID, original string) string {
	serverDigest := sha256.Sum256([]byte(serverID))
	toolPart := strings.Trim(mcpToolNameRE.ReplaceAllString(original, "_"), "_-")
	if toolPart == "" {
		toolPart = "tool"
	}
	toolDigest := sha256.Sum256([]byte(original))
	if len(toolPart) > 38 {
		toolPart = toolPart[:29] + "_" + hex.EncodeToString(toolDigest[:4])
	}
	return "mcp__" + hex.EncodeToString(serverDigest[:5]) + "__" + toolPart
}

func truncateToolName(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}

func publicMCPTools(items []mcpResolvedTool) []domain.MCPTool {
	result := make([]domain.MCPTool, 0, len(items))
	for _, item := range items {
		result = append(result, domain.MCPTool{Name: item.Name, ExposedName: item.ExposedName, Description: item.Description})
	}
	return result
}

func cloneStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[strings.TrimSpace(key)] = value
	}
	return result
}

func sortedMapKeys(source map[string]string) []string {
	result := make([]string, 0, len(source))
	for key := range source {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func mapAsEnvironment(values map[string]string) []string {
	keys := sortedMapKeys(values)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

var _ tool.InvokableTool = (*mcpDynamicTool)(nil)
