package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	posixpath "path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/ids"
	"eino-ops-agent/internal/observability"
	"eino-ops-agent/internal/policy"
	"eino-ops-agent/internal/proxyx"
	"eino-ops-agent/internal/security"
	"eino-ops-agent/internal/skills"
	"eino-ops-agent/internal/sshx"
	"eino-ops-agent/internal/store"
)

type Service struct {
	store                *store.Store
	policy               *policy.Engine
	transport            sshx.Transport
	encryptor            *security.Encryptor
	redactor             *security.Redactor
	limits               config.Limits
	dataDir              string
	workspaceRoot        string
	workspaceSandboxPath string
	workspaceMu          sync.RWMutex
	workspaces           map[string]config.Workspace
	validators           map[string]config.Validator
	skills               *skills.Registry

	globalSem         chan struct{}
	semMu             sync.Mutex
	hostSems          map[string]chan struct{}
	taskMu            sync.RWMutex
	tasks             map[string]*taskState
	explainerMu       sync.RWMutex
	explainer         CommandExplainer
	explainWG         sync.WaitGroup
	explanationMu     sync.Mutex
	explanationActive map[string]*approvalExplanationTask
	explanationSem    chan struct{}
	explanationSlots  chan struct{}
	mcpMu             sync.RWMutex
	mcpRuntime        map[string]*mcpRuntimeState
}

const (
	maxConcurrentApprovalExplanations = 2
	maxQueuedApprovalExplanations     = 4
)

type approvalExplanationTask struct {
	cancel context.CancelFunc
}

type CommandExplainer interface {
	Review(context.Context, domain.CommandReviewInput) (domain.CommandReview, error)
}

type FreshCommandExplainer interface {
	ReviewFresh(context.Context, domain.CommandReviewInput) (domain.CommandReview, error)
}

type taskState struct {
	task   domain.Task
	result domain.ExecResult
	err    string
	cancel context.CancelFunc
}

type HistoryResult struct {
	Run       domain.Run `json:"run"`
	StdoutRaw string     `json:"stdout_raw,omitempty"`
	StderrRaw string     `json:"stderr_raw,omitempty"`
}

func New(st *store.Store, engine *policy.Engine, transport sshx.Transport, encryptor *security.Encryptor, redactor *security.Redactor, limits config.Limits, runtimeConfig ...config.Config) *Service {
	global := limits.GlobalConcurrency
	if global <= 0 {
		global = 8
	}
	result := &Service{
		store: st, policy: engine, transport: transport, encryptor: encryptor, redactor: redactor, limits: limits,
		workspaceSandboxPath: config.Default().WorkspaceSandboxPath,
		globalSem:            make(chan struct{}, global), hostSems: make(map[string]chan struct{}), tasks: make(map[string]*taskState), workspaces: make(map[string]config.Workspace), validators: make(map[string]config.Validator), mcpRuntime: make(map[string]*mcpRuntimeState),
		explanationActive: make(map[string]*approvalExplanationTask), explanationSem: make(chan struct{}, maxConcurrentApprovalExplanations), explanationSlots: make(chan struct{}, maxQueuedApprovalExplanations),
	}
	if len(runtimeConfig) > 0 {
		result.dataDir = runtimeConfig[0].DataDir
		result.workspaceSandboxPath = runtimeConfig[0].WorkspaceSandboxPath
		result.skills = skills.NewRegistry(filepath.Join(result.dataDir, "skills"))
		for _, validator := range runtimeConfig[0].Validators {
			result.validators[validator.ID] = validator
		}
	}
	return result
}

func (s *Service) RecoverInterruptedTasks(ctx context.Context) error {
	return s.store.InterruptActiveTasks(ctx)
}

func (s *Service) Store() *store.Store { return s.store }

func (s *Service) ListChatSessions(ctx context.Context, limit int) ([]domain.ChatSession, error) {
	return s.store.ListChatSessions(ctx, limit)
}

func (s *Service) PrepareChatSession(ctx context.Context, sessionID, workspaceID, actor string) (domain.ChatSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	workspaceID = strings.TrimSpace(workspaceID)
	if sessionID == "" {
		return domain.ChatSession{}, fmt.Errorf("session id is required")
	}
	if workspaceID != "" {
		if _, ok := s.workspaceByID(workspaceID); !ok {
			return domain.ChatSession{}, fmt.Errorf("workspace %q not found", workspaceID)
		}
	}
	session, err := s.store.GetChatSession(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		session, err = s.store.CreateChatSession(ctx, sessionID, workspaceID)
		if err == nil {
			s.audit(ctx, "", "chat_session_created", actor, map[string]any{"session_id": sessionID, "workspace_id": workspaceID})
		}
		return session, err
	}
	if err != nil {
		return domain.ChatSession{}, err
	}
	if workspaceID == "" || session.WorkspaceID == workspaceID {
		return session, nil
	}
	if session.WorkspaceID != "" {
		return domain.ChatSession{}, fmt.Errorf("conversation is bound to workspace %q; switch it before sending a message", session.WorkspaceID)
	}
	return s.SetChatSessionWorkspace(ctx, sessionID, workspaceID, actor)
}

func (s *Service) GetChatSession(ctx context.Context, sessionID string) (domain.ChatSession, error) {
	return s.store.GetChatSession(ctx, strings.TrimSpace(sessionID))
}

func (s *Service) SetChatSessionWorkspace(ctx context.Context, sessionID, workspaceID, actor string) (domain.ChatSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	workspaceID = strings.TrimSpace(workspaceID)
	if sessionID == "" {
		return domain.ChatSession{}, fmt.Errorf("session id is required")
	}
	if workspaceID != "" {
		if _, ok := s.workspaceByID(workspaceID); !ok {
			return domain.ChatSession{}, fmt.Errorf("workspace %q not found", workspaceID)
		}
	}
	current, err := s.store.GetChatSession(ctx, sessionID)
	if err != nil {
		return domain.ChatSession{}, err
	}
	if current.WorkspaceID == workspaceID {
		return current, nil
	}
	session, err := s.store.SetChatSessionWorkspace(ctx, sessionID, workspaceID)
	if err != nil {
		return domain.ChatSession{}, err
	}
	s.audit(ctx, "", "chat_session_workspace_changed", actor, map[string]any{
		"session_id": sessionID, "previous_workspace_id": current.WorkspaceID, "workspace_id": workspaceID,
	})
	return session, nil
}

func (s *Service) SessionWorkspace(ctx context.Context) (WorkspaceCapability, error) {
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		return WorkspaceCapability{}, fmt.Errorf("Workspace tools require an Agent conversation")
	}
	session, err := s.store.GetChatSession(ctx, sessionID)
	if err != nil {
		return WorkspaceCapability{}, fmt.Errorf("load conversation Workspace: %w", err)
	}
	if session.WorkspaceID == "" {
		return WorkspaceCapability{}, fmt.Errorf("no Workspace is bound to this conversation; select one in the chat interface")
	}
	workspace, ok := s.workspaceByID(session.WorkspaceID)
	if !ok {
		return WorkspaceCapability{}, fmt.Errorf("the conversation Workspace %q is no longer available; select another Workspace", session.WorkspaceID)
	}
	return s.adminWorkspaceCapability(workspace).WorkspaceCapability, nil
}

func (s *Service) ListChatMessages(ctx context.Context, sessionID string, limit int) ([]domain.ChatMessage, error) {
	return s.store.ListChatMessages(ctx, sessionID, limit)
}

func (s *Service) GetChatAttachment(ctx context.Context, sessionID, attachmentID string) (domain.ChatAttachment, error) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(attachmentID) == "" {
		return domain.ChatAttachment{}, store.ErrNotFound
	}
	return s.store.GetChatAttachment(ctx, sessionID, attachmentID)
}

func (s *Service) DeleteChatSession(ctx context.Context, sessionID, actor string) error {
	if err := s.store.DeleteChatSession(ctx, sessionID); err != nil {
		return err
	}
	s.audit(ctx, "", "chat_session_deleted", actor, map[string]any{"session_id": sessionID})
	return nil
}

func (s *Service) CreateAgentPlan(ctx context.Context, goal string, titles []string, actor string) (domain.AgentPlan, error) {
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		return domain.AgentPlan{}, fmt.Errorf("agent plan requires a session context")
	}
	goal = strings.TrimSpace(goal)
	if goal == "" || len(goal) > 500 {
		return domain.AgentPlan{}, fmt.Errorf("invalid plan goal: use 1-500 characters")
	}
	if len(titles) < 2 || len(titles) > 8 {
		return domain.AgentPlan{}, fmt.Errorf("invalid plan: provide 2-8 steps")
	}
	steps := make([]domain.AgentPlanStep, 0, len(titles))
	seen := make(map[string]struct{}, len(titles))
	for index, title := range titles {
		title = strings.TrimSpace(title)
		if title == "" || len(title) > 240 {
			return domain.AgentPlan{}, fmt.Errorf("invalid plan step %d: use 1-240 characters", index+1)
		}
		key := strings.ToLower(title)
		if _, exists := seen[key]; exists {
			return domain.AgentPlan{}, fmt.Errorf("invalid plan: duplicate step %q", title)
		}
		seen[key] = struct{}{}
		status := "pending"
		if index == 0 {
			status = "in_progress"
		}
		steps = append(steps, domain.AgentPlanStep{Number: index + 1, Title: title, Status: status})
	}
	plan, err := s.store.ReplaceAgentPlan(ctx, domain.AgentPlan{SessionID: sessionID, Goal: goal, Status: "active", Steps: steps})
	if err != nil {
		return domain.AgentPlan{}, err
	}
	s.audit(ctx, "", "agent_plan_created", actor, map[string]any{"session_id": sessionID, "goal": goal, "step_count": len(steps)})
	observability.FromContext(ctx).InfoContext(ctx, "agent plan created", "component", "agent", "session_id", sessionID, "step_count", len(steps))
	return plan, nil
}

func (s *Service) GetAgentPlan(ctx context.Context, sessionID string) (domain.AgentPlan, error) {
	if strings.TrimSpace(sessionID) == "" {
		sessionID = SessionIDFromContext(ctx)
	}
	if sessionID == "" {
		return domain.AgentPlan{}, fmt.Errorf("agent plan requires a session context")
	}
	return s.store.GetAgentPlan(ctx, sessionID)
}

func (s *Service) UpdateAgentPlanStep(ctx context.Context, stepNumber int, status, evidence, actor string) (domain.AgentPlan, error) {
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		return domain.AgentPlan{}, fmt.Errorf("agent plan requires a session context")
	}
	if stepNumber < 1 || stepNumber > 8 {
		return domain.AgentPlan{}, fmt.Errorf("invalid plan step number")
	}
	status = strings.TrimSpace(status)
	if status != "completed" && status != "blocked" {
		return domain.AgentPlan{}, fmt.Errorf("invalid plan step status: use completed or blocked")
	}
	evidence = strings.TrimSpace(evidence)
	if evidence == "" || len(evidence) > 2000 {
		return domain.AgentPlan{}, fmt.Errorf("invalid step evidence: use 1-2000 characters")
	}
	plan, err := s.store.AdvanceAgentPlan(ctx, sessionID, stepNumber, status, evidence)
	if err != nil {
		return domain.AgentPlan{}, err
	}
	s.audit(ctx, "", "agent_plan_step_updated", actor, map[string]any{
		"session_id": sessionID, "step_number": stepNumber, "status": status, "plan_status": plan.Status,
	})
	observability.FromContext(ctx).InfoContext(ctx, "agent plan step updated", "component", "agent", "session_id", sessionID, "step_number", stepNumber, "status", status, "plan_status", plan.Status)
	return plan, nil
}

func (s *Service) SaveModelProvider(ctx context.Context, input domain.ModelProviderInput, actor string) (domain.ModelProvider, error) {
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Kind = strings.TrimSpace(input.Kind)
	input.BaseURL = strings.TrimSpace(input.BaseURL)
	input.Model = strings.TrimSpace(input.Model)
	input.ProxyUsername = strings.TrimSpace(input.ProxyUsername)
	userAgent := ""
	if input.UserAgent != nil {
		normalizedUserAgent, err := validateProviderUserAgent(*input.UserAgent)
		if err != nil {
			return domain.ModelProvider{}, err
		}
		userAgent = normalizedUserAgent
	}
	if input.Name == "" {
		return domain.ModelProvider{}, fmt.Errorf("provider name is required")
	}
	if input.Model == "" {
		return domain.ModelProvider{}, fmt.Errorf("model is required")
	}
	if input.Kind == "" {
		input.Kind = "openai_compatible"
	}
	switch input.Kind {
	case "openai", "deepseek", "anthropic", "openai_compatible", "ollama":
	default:
		return domain.ModelProvider{}, fmt.Errorf("invalid provider kind %q", input.Kind)
	}
	normalizedBaseURL, err := normalizeProviderBaseURL(input.BaseURL, input.Kind)
	if err != nil {
		return domain.ModelProvider{}, err
	}
	input.BaseURL = normalizedBaseURL
	input.ProxyURL, err = proxyx.NormalizeURL(input.ProxyURL)
	if err != nil {
		return domain.ModelProvider{}, err
	}
	if len(input.ProxyURL) > 2048 {
		return domain.ModelProvider{}, fmt.Errorf("proxy URL is too long")
	}
	if containsCredentialControl(input.ProxyUsername) || containsCredentialControl(input.ProxyPassword) {
		return domain.ModelProvider{}, fmt.Errorf("proxy credentials cannot contain NUL, carriage return, or newline characters")
	}
	if len(input.ProxyUsername) > 255 || len(input.ProxyPassword) > 255 {
		return domain.ModelProvider{}, fmt.Errorf("proxy credentials are too long")
	}
	if input.ProxyURL == "" {
		input.ProxyUsername = ""
		input.ProxyPassword = ""
	}

	provider := domain.ModelProvider{
		ID: input.ID, Name: input.Name, Kind: input.Kind, BaseURL: input.BaseURL, Model: input.Model,
		ProxyURL: input.ProxyURL, ProxyUsername: input.ProxyUsername, UserAgent: userAgent,
	}
	if input.ID != "" {
		existing, err := s.store.GetModelProvider(ctx, input.ID)
		if err != nil {
			return domain.ModelProvider{}, err
		}
		provider.CreatedAt = existing.CreatedAt
		provider.Active = existing.Active
		provider.APIKeyCipher = existing.APIKeyCipher
		if input.UserAgent == nil {
			provider.UserAgent = existing.UserAgent
		}
		if provider.ProxyURL == existing.ProxyURL && provider.ProxyUsername == existing.ProxyUsername {
			provider.ProxyPasswordCipher = existing.ProxyPasswordCipher
		}
	}
	if key := strings.TrimSpace(input.APIKey); key != "" {
		cipher, err := s.encryptor.Encrypt([]byte(key))
		if err != nil {
			return domain.ModelProvider{}, err
		}
		provider.APIKeyCipher = cipher
	}
	if providerKindRequiresAPIKey(provider.Kind) && provider.APIKeyCipher == "" {
		return domain.ModelProvider{}, fmt.Errorf("api_key is required for %s", provider.Kind)
	}
	if input.ClearProxyPassword || provider.ProxyURL == "" || provider.ProxyUsername == "" {
		provider.ProxyPasswordCipher = ""
	} else if input.ProxyPassword != "" {
		cipher, err := s.encryptor.Encrypt([]byte(input.ProxyPassword))
		if err != nil {
			return domain.ModelProvider{}, fmt.Errorf("encrypt model provider proxy password: %w", err)
		}
		provider.ProxyPasswordCipher = cipher
	}
	saved, err := s.store.UpsertModelProvider(ctx, provider)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
			return domain.ModelProvider{}, fmt.Errorf("provider name already exists")
		}
		return domain.ModelProvider{}, err
	}
	s.audit(ctx, "", "model_provider_saved", actor, map[string]any{
		"provider_id": saved.ID, "name": saved.Name, "kind": saved.Kind, "model": saved.Model,
		"proxy_configured": saved.ProxyURL != "",
	})
	return saved, nil
}

func (s *Service) ListModelProviders(ctx context.Context) ([]domain.ModelProvider, error) {
	return s.store.ListModelProviders(ctx)
}

func (s *Service) SystemSettings(ctx context.Context) (domain.SystemSettings, error) {
	settings, err := s.store.GetSystemSettings(ctx)
	if err != nil {
		return domain.SystemSettings{}, err
	}
	return s.decorateWorkspaceShellSettings(settings), nil
}

func (s *Service) SaveSystemSettings(ctx context.Context, input domain.SystemSettingsInput, actor string) (domain.SystemSettings, error) {
	if input.AgentMaxIterations < domain.MinAgentMaxIterations || input.AgentMaxIterations > domain.MaxAgentMaxIterations {
		return domain.SystemSettings{}, fmt.Errorf("agent_max_iterations must be between %d and %d", domain.MinAgentMaxIterations, domain.MaxAgentMaxIterations)
	}
	current, err := s.store.GetSystemSettings(ctx)
	if err != nil {
		return domain.SystemSettings{}, err
	}
	current.AgentMaxIterations = input.AgentMaxIterations
	systemPromptChanged := false
	if input.SystemPrompt != nil {
		systemPromptChanged = current.SystemPrompt != *input.SystemPrompt
		current.SystemPrompt = *input.SystemPrompt
	}
	if input.ApprovalExplanationsEnabled != nil {
		current.ApprovalExplanationsEnabled = *input.ApprovalExplanationsEnabled
	}
	if input.SubagentModelProviderID != nil {
		providerID := strings.TrimSpace(*input.SubagentModelProviderID)
		if providerID != "" {
			if _, err := s.store.GetModelProvider(ctx, providerID); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return domain.SystemSettings{}, fmt.Errorf("subagent model provider %q not found", providerID)
				}
				return domain.SystemSettings{}, err
			}
		}
		current.SubagentModelProviderID = providerID
	}
	if input.SubagentTimeoutSeconds != nil {
		if *input.SubagentTimeoutSeconds < domain.MinSubagentTimeoutSeconds || *input.SubagentTimeoutSeconds > domain.MaxSubagentTimeoutSeconds {
			return domain.SystemSettings{}, fmt.Errorf("subagent_timeout_seconds must be between %d and %d", domain.MinSubagentTimeoutSeconds, domain.MaxSubagentTimeoutSeconds)
		}
		current.SubagentTimeoutSeconds = *input.SubagentTimeoutSeconds
	}
	if input.ChatImageAllowedTypes != nil {
		allowed := map[string]struct{}{
			"image/png": {}, "image/jpeg": {}, "image/webp": {}, "image/gif": {},
		}
		seen := make(map[string]struct{}, len(input.ChatImageAllowedTypes))
		normalized := make([]string, 0, len(input.ChatImageAllowedTypes))
		for _, value := range input.ChatImageAllowedTypes {
			value = strings.ToLower(strings.TrimSpace(value))
			if _, ok := allowed[value]; !ok {
				return domain.SystemSettings{}, fmt.Errorf("unsupported chat image type %q", value)
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			normalized = append(normalized, value)
		}
		if len(normalized) == 0 {
			return domain.SystemSettings{}, fmt.Errorf("at least one chat image type is required")
		}
		current.ChatImageAllowedTypes = normalized
	}
	if input.WorkspaceShellMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*input.WorkspaceShellMode))
		switch mode {
		case domain.WorkspaceShellModeSandbox, domain.WorkspaceShellModeHost, domain.WorkspaceShellModeDisabled:
			current.WorkspaceShellMode = mode
		default:
			return domain.SystemSettings{}, fmt.Errorf("workspace_shell_mode must be sandbox, host, or disabled")
		}
	}
	saved, err := s.store.SaveSystemSettings(ctx, current)
	if err != nil {
		return domain.SystemSettings{}, err
	}
	s.audit(ctx, "", "system_settings_updated", actor, map[string]any{
		"agent_max_iterations": saved.AgentMaxIterations, "approval_explanations_enabled": saved.ApprovalExplanationsEnabled,
		"system_prompt_changed": systemPromptChanged, "system_prompt_bytes": len(saved.SystemPrompt),
		"subagent_model_provider_id": saved.SubagentModelProviderID, "subagent_timeout_seconds": saved.SubagentTimeoutSeconds,
		"chat_image_allowed_types": saved.ChatImageAllowedTypes,
		"workspace_shell_mode":     saved.WorkspaceShellMode,
	})
	return s.decorateWorkspaceShellSettings(saved), nil
}

func (s *Service) SetCommandExplainer(explainer CommandExplainer) {
	s.explainerMu.Lock()
	s.explainer = explainer
	s.explainerMu.Unlock()
}

func (s *Service) commandExplainer() CommandExplainer {
	s.explainerMu.RLock()
	defer s.explainerMu.RUnlock()
	return s.explainer
}

func (s *Service) registerApprovalExplanation(approvalID string, task *approvalExplanationTask) {
	s.explanationMu.Lock()
	previous := s.explanationActive[approvalID]
	s.explanationActive[approvalID] = task
	s.explanationMu.Unlock()
	if previous != nil {
		previous.cancel()
	}
}

func (s *Service) clearApprovalExplanation(approvalID string, task *approvalExplanationTask) {
	s.explanationMu.Lock()
	if s.explanationActive[approvalID] == task {
		delete(s.explanationActive, approvalID)
	}
	s.explanationMu.Unlock()
}

func (s *Service) cancelApprovalExplanation(ctx context.Context, approvalID, runID string) bool {
	s.explanationMu.Lock()
	task := s.explanationActive[approvalID]
	if task != nil {
		delete(s.explanationActive, approvalID)
	}
	s.explanationMu.Unlock()
	if task == nil {
		return false
	}
	task.cancel()
	if runID != "" {
		_ = s.store.UpdateRunAIReview(ctx, runID, "")
	}
	return true
}

func (s *Service) ModelProviderConfig(ctx context.Context, id string) (config.Model, domain.ModelProvider, error) {
	provider, err := s.store.GetModelProvider(ctx, id)
	if err != nil {
		return config.Model{}, domain.ModelProvider{}, err
	}
	key, err := s.encryptor.Decrypt(provider.APIKeyCipher)
	if err != nil {
		return config.Model{}, domain.ModelProvider{}, fmt.Errorf("decrypt model provider API key: %w", err)
	}
	proxyPassword, err := s.encryptor.Decrypt(provider.ProxyPasswordCipher)
	if err != nil {
		return config.Model{}, domain.ModelProvider{}, fmt.Errorf("decrypt model provider proxy password: %w", err)
	}
	return config.Model{
		APIKey: string(key), Kind: provider.Kind, BaseURL: provider.BaseURL, Name: provider.Model, UserAgent: provider.UserAgent,
		ProxyURL: provider.ProxyURL, ProxyUsername: provider.ProxyUsername, ProxyPassword: string(proxyPassword),
	}, provider, nil
}

func (s *Service) ActiveModelConfig(ctx context.Context) (config.Model, domain.ModelProvider, error) {
	provider, err := s.store.ActiveModelProvider(ctx)
	if err != nil {
		return config.Model{}, domain.ModelProvider{}, err
	}
	return s.ModelProviderConfig(ctx, provider.ID)
}

func (s *Service) ActivateModelProvider(ctx context.Context, id, actor string) (domain.ModelProvider, error) {
	provider, err := s.store.GetModelProvider(ctx, id)
	if err != nil {
		return domain.ModelProvider{}, err
	}
	if err := s.store.ActivateModelProvider(ctx, id); err != nil {
		return domain.ModelProvider{}, err
	}
	provider.Active = true
	s.audit(ctx, "", "model_provider_activated", actor, map[string]any{
		"provider_id": provider.ID, "name": provider.Name, "model": provider.Model,
	})
	return provider, nil
}

func (s *Service) DeleteModelProvider(ctx context.Context, id, actor string) (bool, error) {
	provider, err := s.store.GetModelProvider(ctx, id)
	if err != nil {
		return false, err
	}
	settings, err := s.store.GetSystemSettings(ctx)
	if err != nil {
		return false, err
	}
	if settings.SubagentModelProviderID == provider.ID {
		return false, fmt.Errorf("%w: %q is selected for the subagent; choose another provider in system settings before deleting it", ErrModelProviderInUse, provider.Name)
	}
	if err := s.store.DeleteModelProvider(ctx, id); err != nil {
		return false, err
	}
	s.audit(ctx, "", "model_provider_deleted", actor, map[string]any{
		"provider_id": provider.ID, "name": provider.Name, "was_active": provider.Active,
	})
	return provider.Active, nil
}

func (s *Service) AddHost(ctx context.Context, host domain.Host, actor string) (domain.Host, error) {
	return s.SaveHost(ctx, domain.HostInput{
		ID: host.ID, Name: host.Name, Address: host.Address, Port: host.Port, User: host.User,
		AuthType: host.AuthType, KnownHostsFile: host.KnownHostsFile, ProxyJumpHostID: host.ProxyJumpHostID,
		ProxyURL: host.ProxyURL, ProxyUsername: host.ProxyUsername, SudoMode: host.SudoMode,
	}, actor)
}

func (s *Service) SaveHost(ctx context.Context, input domain.HostInput, actor string) (domain.Host, error) {
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Address = strings.TrimSpace(input.Address)
	input.User = strings.TrimSpace(input.User)
	input.AuthType = strings.TrimSpace(input.AuthType)
	input.KnownHostsFile = strings.TrimSpace(input.KnownHostsFile)
	input.ProxyJumpHostID = strings.TrimSpace(input.ProxyJumpHostID)
	input.ProxyUsername = strings.TrimSpace(input.ProxyUsername)
	input.SudoMode = strings.TrimSpace(input.SudoMode)
	var existing domain.Host
	hasExisting := false
	if input.ID != "" {
		var err error
		existing, err = s.store.GetHost(ctx, input.ID)
		if err != nil {
			return domain.Host{}, err
		}
		hasExisting = true
	}
	if input.Name == "" {
		return domain.Host{}, fmt.Errorf("host name is required")
	}
	if input.Port == 0 {
		input.Port = 22
	}
	if input.Port < 1 || input.Port > 65535 {
		return domain.Host{}, fmt.Errorf("invalid SSH port")
	}
	if input.AuthType == "" {
		input.AuthType = "agent"
	}
	if input.SudoMode == "" {
		input.SudoMode = "none"
	}
	if input.Address == "" || input.User == "" {
		return domain.Host{}, fmt.Errorf("address and user are required")
	}
	proxyURL, err := sshx.NormalizeProxyURL(input.ProxyURL)
	if err != nil {
		return domain.Host{}, err
	}
	input.ProxyURL = proxyURL
	if input.ProxyURL == "" {
		input.ProxyUsername = ""
		input.ProxyPassword = ""
	}
	if input.AuthType == "key" && input.PrivateKey == "" && (!hasExisting || existing.PrivateKeyCipher == "") {
		return domain.Host{}, fmt.Errorf("private_key upload is required for key authentication")
	}
	switch input.AuthType {
	case "agent", "key", "password":
	default:
		return domain.Host{}, fmt.Errorf("invalid SSH authentication type %q", input.AuthType)
	}
	switch input.SudoMode {
	case "none", "nopasswd", "password":
	default:
		return domain.Host{}, fmt.Errorf("invalid sudo mode %q", input.SudoMode)
	}
	if containsCredentialControl(input.Password) || containsCredentialControl(input.SudoPassword) || containsCredentialControl(input.ProxyUsername) || containsCredentialControl(input.ProxyPassword) {
		return domain.Host{}, fmt.Errorf("credentials cannot contain NUL, carriage return, or newline characters")
	}
	if len(input.Password) > 1024 || len(input.SudoPassword) > 1024 {
		return domain.Host{}, fmt.Errorf("password is too long")
	}
	if len(input.ProxyUsername) > 255 || len(input.ProxyPassword) > 255 {
		return domain.Host{}, fmt.Errorf("proxy credentials are too long")
	}
	if input.AuthType != "key" {
		input.PrivateKey = ""
	}

	host := domain.Host{
		ID: input.ID, Name: input.Name, Address: input.Address, Port: input.Port, User: input.User,
		AuthType: input.AuthType, KnownHostsFile: input.KnownHostsFile, ProxyJumpHostID: input.ProxyJumpHostID,
		ProxyURL: input.ProxyURL, ProxyUsername: input.ProxyUsername, SudoMode: input.SudoMode,
	}
	if hasExisting {
		host.CreatedAt = existing.CreatedAt
		host.PasswordCipher = existing.PasswordCipher
		host.SudoCipher = existing.SudoCipher
		host.PrivateKeyCipher = existing.PrivateKeyCipher
		if input.ProxyURL == existing.ProxyURL && input.ProxyUsername == existing.ProxyUsername {
			host.ProxyPasswordCipher = existing.ProxyPasswordCipher
		}
	}
	if input.AuthType != "key" {
		host.PrivateKeyCipher = ""
	} else if input.PrivateKey != "" {
		privateKey := []byte(input.PrivateKey)
		if err := sshx.ValidatePrivateKey(privateKey); err != nil {
			return domain.Host{}, fmt.Errorf("invalid SSH private key upload: %w", err)
		}
		cipher, err := s.encryptor.Encrypt(privateKey)
		if err != nil {
			return domain.Host{}, fmt.Errorf("encrypt SSH private key: %w", err)
		}
		host.PrivateKeyCipher = cipher
	}
	if input.AuthType != "password" {
		host.PasswordCipher = ""
	} else if input.Password != "" {
		cipher, err := s.encryptor.Encrypt([]byte(input.Password))
		if err != nil {
			return domain.Host{}, fmt.Errorf("encrypt SSH password: %w", err)
		}
		host.PasswordCipher = cipher
	}
	if input.SudoMode != "password" {
		host.SudoCipher = ""
	} else if input.SudoPassword != "" {
		cipher, err := s.encryptor.Encrypt([]byte(input.SudoPassword))
		if err != nil {
			return domain.Host{}, fmt.Errorf("encrypt sudo password: %w", err)
		}
		host.SudoCipher = cipher
	}
	if input.ProxyURL == "" || input.ProxyUsername == "" {
		host.ProxyPasswordCipher = ""
	} else if input.ProxyPassword != "" {
		cipher, err := s.encryptor.Encrypt([]byte(input.ProxyPassword))
		if err != nil {
			return domain.Host{}, fmt.Errorf("encrypt SSH proxy password: %w", err)
		}
		host.ProxyPasswordCipher = cipher
	}
	if input.AuthType == "password" && host.PasswordCipher == "" {
		return domain.Host{}, fmt.Errorf("password is required for password authentication")
	}
	if input.SudoMode == "password" && host.SudoCipher == "" {
		return domain.Host{}, fmt.Errorf("sudo_password is required for password sudo mode")
	}
	if input.ProxyJumpHostID != "" {
		if input.ProxyJumpHostID == input.ID && input.ID != "" {
			return domain.Host{}, fmt.Errorf("a host cannot use itself as ProxyJump")
		}
		_, err := s.store.GetHost(ctx, input.ProxyJumpHostID)
		if err != nil {
			return domain.Host{}, fmt.Errorf("load ProxyJump host %q: %w", input.ProxyJumpHostID, err)
		}
	}

	created, err := s.store.UpsertHost(ctx, host)
	if err != nil {
		return domain.Host{}, err
	}
	s.audit(ctx, "", "host_saved", actor, map[string]any{
		"host_id": created.ID, "name": created.Name, "auth_type": created.AuthType, "has_private_key": created.HasPrivateKey, "sudo_mode": created.SudoMode,
	})
	return created, nil
}

func (s *Service) GetHost(ctx context.Context, id string) (domain.Host, error) {
	return s.store.GetHost(ctx, id)
}

func (s *Service) ListHosts(ctx context.Context) ([]domain.Host, error) {
	return s.store.ListHosts(ctx)
}

func (s *Service) ListHostCapabilities(ctx context.Context) ([]domain.HostCapability, error) {
	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]domain.HostCapability, 0, len(hosts))
	for _, host := range hosts {
		result = append(result, domain.HostCapability{ID: host.ID, Name: host.Name, AuthType: host.AuthType, SudoMode: host.SudoMode})
	}
	return result, nil
}

func (s *Service) DeleteHost(ctx context.Context, id, actor string) error {
	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return err
	}
	for _, host := range hosts {
		if host.ProxyJumpHostID == id {
			return fmt.Errorf("host %q is still used as ProxyJump by %q", id, host.Name)
		}
	}
	if err := s.store.DeleteHost(ctx, id); err != nil {
		return err
	}
	s.audit(ctx, "", "host_deleted", actor, map[string]any{"host_id": id})
	return nil
}

func (s *Service) ProbeHost(ctx context.Context, id string) (sshx.HostInfo, error) {
	host, err := s.store.GetHost(ctx, id)
	if err != nil {
		return sshx.HostInfo{}, err
	}
	connection, _, err := s.resolveSSHConnection(ctx, host)
	if err != nil {
		return sshx.HostInfo{}, err
	}
	connection, err = s.hydrateSSHConnection(connection, false)
	if err != nil {
		return sshx.HostInfo{}, err
	}
	return s.transport.Probe(ctx, connection)
}

func (s *Service) ScanHostKey(ctx context.Context, id string) (sshx.HostKey, error) {
	host, err := s.store.GetHost(ctx, id)
	if err != nil {
		return sshx.HostKey{}, err
	}
	connection, _, err := s.resolveSSHConnection(ctx, host)
	if err != nil {
		return sshx.HostKey{}, err
	}
	connection, err = s.hydrateSSHConnection(connection, false)
	if err != nil {
		return sshx.HostKey{}, err
	}
	return s.transport.ScanHostKey(ctx, connection)
}

func (s *Service) TrustHostKey(ctx context.Context, id, fingerprint, actor string) (sshx.HostKey, error) {
	host, err := s.store.GetHost(ctx, id)
	if err != nil {
		return sshx.HostKey{}, err
	}
	connection, _, err := s.resolveSSHConnection(ctx, host)
	if err != nil {
		return sshx.HostKey{}, err
	}
	connection, err = s.hydrateSSHConnection(connection, false)
	if err != nil {
		return sshx.HostKey{}, err
	}
	key, err := s.transport.TrustHostKey(ctx, connection, fingerprint)
	if err == nil {
		s.audit(ctx, "", "host_key_trusted", actor, map[string]any{"host_id": id, "fingerprint": key.Fingerprint})
	}
	return key, err
}

func (s *Service) Evaluate(ctx context.Context, req domain.ExecRequest) (domain.Decision, error) {
	host, err := s.store.GetHost(ctx, req.HostID)
	if err != nil {
		return domain.Decision{}, err
	}
	normalizeRequest(&req, s.limits)
	if err := validateRequestLimits(req, s.limits, s.redactor); err != nil {
		return domain.Decision{}, err
	}
	if req.Mode == domain.ExecWorkspaceUpload {
		if _, err := s.prepareWorkspaceUpload(req); err != nil {
			return domain.Decision{}, err
		}
	}
	var transferSource domain.Host
	if isWorkspaceMode(req.Mode) {
		if req.SSHConnectionDigest != "" || req.SourceConnectionDigest != "" {
			return domain.Decision{}, fmt.Errorf("SSH connection binding is invalid for local Workspace operations")
		}
	} else if req.Mode == domain.ExecSSHFileTransfer {
		transferSource, err = s.bindSSHFileTransfer(ctx, host, &req)
		if err != nil {
			return domain.Decision{}, err
		}
	} else {
		if req.SourceConnectionDigest != "" {
			return domain.Decision{}, fmt.Errorf("source SSH connection binding is only valid for host-to-host transfers")
		}
		_, digest, err := s.resolveSSHConnection(ctx, host)
		if err != nil {
			return domain.Decision{}, err
		}
		bindSSHRequest(&req, digest)
	}
	if err := validateExecutionRequest(host, req); err != nil {
		return domain.Decision{}, err
	}
	decision := s.policy.Evaluate(ctx, host, req)
	if req.Mode == domain.ExecSSHFileTransfer {
		decision = mergeTransferDecisions(decision, s.policy.Evaluate(ctx, transferSource, req))
	}
	return decision, nil
}

func (s *Service) Submit(ctx context.Context, req domain.ExecRequest, actor string) (domain.ExecResult, error) {
	result, err := s.submit(ctx, req, actor, nil)
	if err != nil || !blockingApprovalsFromContext(ctx) || result.Status != "approval_required" || result.ApprovalID == "" {
		return result, err
	}
	notifyApproval(ctx, result)
	return s.awaitApproval(ctx, result)
}

func (s *Service) submit(ctx context.Context, req domain.ExecRequest, actor string, stream func(string, []byte)) (domain.ExecResult, error) {
	normalizeRequest(&req, s.limits)
	if err := validateRequestLimits(req, s.limits, s.redactor); err != nil {
		return domain.ExecResult{}, err
	}
	if strings.TrimSpace(req.Reason) == "" {
		return domain.ExecResult{}, fmt.Errorf("reason is required")
	}
	if req.Mode == domain.ExecWorkspaceUpload {
		if _, err := s.prepareWorkspaceUpload(req); err != nil {
			return domain.ExecResult{}, err
		}
	}
	host, err := s.store.GetHost(ctx, req.HostID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	var transferSource domain.Host
	if isWorkspaceMode(req.Mode) {
		if req.SSHConnectionDigest != "" || req.SourceConnectionDigest != "" {
			return domain.ExecResult{}, fmt.Errorf("SSH connection binding is invalid for local Workspace operations")
		}
	} else if req.Mode == domain.ExecSSHFileTransfer {
		transferSource, err = s.bindSSHFileTransfer(ctx, host, &req)
		if err != nil {
			return domain.ExecResult{}, err
		}
	} else {
		if req.SourceConnectionDigest != "" {
			return domain.ExecResult{}, fmt.Errorf("source SSH connection binding is only valid for host-to-host transfers")
		}
		_, digest, connectionErr := s.resolveSSHConnection(ctx, host)
		if connectionErr != nil {
			return domain.ExecResult{}, connectionErr
		}
		bindSSHRequest(&req, digest)
	}
	if err := validateExecutionRequest(host, req); err != nil {
		return domain.ExecResult{}, err
	}
	requestJSON, digest, err := canonicalRequest(req)
	if err != nil {
		return domain.ExecResult{}, err
	}
	decision := s.policy.Evaluate(ctx, host, req)
	if req.Mode == domain.ExecSSHFileTransfer {
		decision = mergeTransferDecisions(decision, s.policy.Evaluate(ctx, transferSource, req))
	}
	sessionID := SessionIDFromContext(ctx)
	sessionGrantUsed := false
	// A session grant can only remove repeated Change-level prompts. Critical
	// requests always require a fresh one-time approval, even if policy is
	// tightened after an earlier grant was created for the same request shape.
	if decision.Action == domain.ActionApprove && decision.Risk == domain.RiskChange && sessionID != "" && !isHostWorkspaceShell(req) {
		fingerprint, fingerprintErr := approvalFingerprint(req)
		if fingerprintErr != nil {
			return domain.ExecResult{}, fingerprintErr
		}
		granted, grantErr := s.store.HasSessionApprovalGrant(ctx, sessionID, fingerprint)
		if grantErr != nil {
			return domain.ExecResult{}, grantErr
		}
		if granted {
			decision.Action = domain.ActionAllow
			decision.Reason = "approved by an exact-operation grant in this Agent session"
			decision.RuleHits = append(decision.RuleHits, "session_approval_grant")
			sessionGrantUsed = true
		}
	}
	requestCipher, err := s.encryptor.Encrypt([]byte(requestJSON))
	if err != nil {
		return domain.ExecResult{}, err
	}
	requestRedacted := s.redactor.Redact(requestJSON)
	now := time.Now().UTC()
	var commandExplanation *domain.CommandReview
	var explanationInput *domain.CommandReviewInput
	var explainer CommandExplainer
	settings, settingsErr := s.store.GetSystemSettings(ctx)
	if settingsErr != nil {
		return domain.ExecResult{}, settingsErr
	}
	if settings.ApprovalExplanationsEnabled && (decision.Action == domain.ActionApprove || decision.Action == domain.ActionBreakGlass) {
		if explainer = s.commandExplainer(); explainer != nil {
			planStep := ""
			if sessionID != "" {
				if plan, planErr := s.store.GetAgentPlan(ctx, sessionID); planErr == nil {
					for _, step := range plan.Steps {
						if step.Status == "in_progress" {
							planStep = fmt.Sprintf("%d. %s", step.Number, step.Title)
							break
						}
					}
				}
			}
			input := domain.CommandReviewInput{
				Request: req, Policy: decision, Host: domain.HostCapability{ID: host.ID, Name: host.Name, AuthType: host.AuthType, SudoMode: host.SudoMode},
				PlanStep: planStep, RequestDigest: digest,
			}
			explanationInput = &input
			commandExplanation = &domain.CommandReview{
				Status: "pending", DeterministicRisk: decision.Risk,
			}
		}
	}
	reviewJSON := ""
	if commandExplanation != nil {
		if encoded, marshalErr := json.Marshal(commandExplanation); marshalErr == nil {
			reviewJSON = string(encoded)
		}
	}
	run := domain.Run{
		ID: ids.New("run"), SessionID: sessionID, HostID: host.ID, RequestJSON: requestRedacted, RequestCipher: requestCipher,
		SearchText: s.redactor.Redact(req.SearchText()), RequestDigest: digest,
		Risk: decision.Risk, Status: "created", AIReviewJSON: reviewJSON, AIReview: commandExplanation, StartedAt: now,
	}
	logger := observability.FromContext(ctx).With(
		"session_id", sessionID, "host_id", host.ID,
		"mode", req.Mode, "program", req.Program, "elevated", req.Elevated,
		"actor", actor, "run_id", run.ID,
	)
	policyLogger := logger.With("component", "policy")
	policyLogger.DebugContext(ctx, "execution policy evaluated", "risk", decision.Risk, "action", decision.Action, "policy_hit_count", len(decision.RuleHits), "request_digest", digest)
	switch decision.Action {
	case domain.ActionDeny:
		run.Status = "denied"
		run.Error = decision.Reason
		run.CompletedAt = now
		if err := s.store.CreateRun(ctx, run); err != nil {
			return domain.ExecResult{}, err
		}
		_ = s.store.UpdateRun(ctx, run)
		s.audit(ctx, run.ID, "command_denied", actor, map[string]any{"risk": decision.Risk, "hits": decision.RuleHits})
		policyLogger.WarnContext(ctx, "execution denied by policy", "risk", decision.Risk, "policy_hit_count", len(decision.RuleHits))
		return domain.ExecResult{RunID: run.ID, Status: run.Status, Risk: decision.Risk, PolicyHits: decision.RuleHits}, nil
	case domain.ActionApprove, domain.ActionBreakGlass:
		run.Status = "approval_required"
		if err := s.store.CreateRun(ctx, run); err != nil {
			return domain.ExecResult{}, err
		}
		approval := domain.Approval{
			ID: ids.New("approval"), RunID: run.ID, HostID: host.ID, RequestJSON: requestRedacted, RequestCipher: requestCipher,
			RequestDigest: digest, Risk: decision.Risk, Status: "pending",
			CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute),
		}
		if err := s.store.CreateApproval(ctx, approval); err != nil {
			return domain.ExecResult{}, err
		}
		s.audit(ctx, run.ID, "approval_requested", actor, map[string]any{"approval_id": approval.ID, "risk": decision.Risk, "hits": decision.RuleHits})
		logger.With("component", "approval").InfoContext(ctx, "execution awaiting approval", "approval_id", approval.ID, "risk", decision.Risk, "expires_at", approval.ExpiresAt)
		if explanationInput != nil && explainer != nil {
			s.startPendingApprovalExplanation(ctx, approval, *explanationInput, explainer, settings.SubagentTimeoutSeconds)
		}
		return domain.ExecResult{RunID: run.ID, Status: run.Status, Risk: decision.Risk, ApprovalID: approval.ID, PolicyHits: decision.RuleHits}, nil
	case domain.ActionAllow:
		run.Status = "running"
		if err := s.store.CreateRun(ctx, run); err != nil {
			return domain.ExecResult{}, err
		}
		if sessionGrantUsed {
			s.audit(ctx, run.ID, "session_approval_grant_used", actor, map[string]any{"session_id": sessionID})
		}
		return s.execute(ctx, host, req, run, actor, decision.RuleHits, stream)
	default:
		return domain.ExecResult{}, fmt.Errorf("unsupported policy decision %q", decision.Action)
	}
}

// startPendingApprovalExplanation keeps model latency outside the human
// approval critical path. Explanation work is bounded globally and canceled as
// soon as its approval is no longer pending.
func (s *Service) startPendingApprovalExplanation(parent context.Context, approval domain.Approval, input domain.CommandReviewInput, explainer CommandExplainer, timeoutSeconds int) {
	baseCtx := context.WithoutCancel(parent)
	timeoutSeconds = effectiveSubagentTimeoutSeconds(timeoutSeconds)
	logger := observability.FromContext(baseCtx).With(
		"component", "approval", "approval_id", approval.ID, "run_id", approval.RunID,
	)
	select {
	case s.explanationSlots <- struct{}{}:
	default:
		review := domain.CommandReview{
			Status: "unavailable", DeterministicRisk: input.Policy.Risk,
			Errors: []string{"command explanation skipped because the local queue is full"}, ReviewedAt: time.Now().UTC(),
		}
		persistCtx, cancelPersist := context.WithTimeout(baseCtx, 3*time.Second)
		err := s.persistPendingApprovalExplanation(persistCtx, approval, input.Policy.Risk, review, 0)
		cancelPersist()
		if err != nil {
			logger.ErrorContext(baseCtx, "persist skipped approval explanation failed", "error", err)
		} else {
			logger.WarnContext(baseCtx, "approval explanation skipped", "reason", "queue_full")
		}
		return
	}

	queuedAt := time.Now()
	explanationCtx, cancelExplanation := context.WithTimeout(baseCtx, time.Duration(timeoutSeconds)*time.Second)
	task := &approvalExplanationTask{cancel: cancelExplanation}
	s.registerApprovalExplanation(approval.ID, task)
	s.explainWG.Add(1)
	go func() {
		defer s.explainWG.Done()
		defer func() { <-s.explanationSlots }()
		defer cancelExplanation()
		defer s.clearApprovalExplanation(approval.ID, task)
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.ErrorContext(baseCtx, "approval explanation Agent panicked", "panic", fmt.Sprint(recovered))
			}
		}()

		select {
		case s.explanationSem <- struct{}{}:
			defer func() { <-s.explanationSem }()
		case <-explanationCtx.Done():
			if errors.Is(explanationCtx.Err(), context.Canceled) {
				logger.InfoContext(baseCtx, "approval explanation canceled while queued", "queue_ms", time.Since(queuedAt).Milliseconds())
				return
			}
			review := s.normalizeCommandReview(domain.CommandReview{}, explanationCtx.Err(), input.Policy.Risk, timeoutSeconds)
			persistCtx, cancelPersist := context.WithTimeout(baseCtx, 3*time.Second)
			err := s.persistPendingApprovalExplanation(persistCtx, approval, input.Policy.Risk, review, time.Since(queuedAt))
			cancelPersist()
			if err != nil {
				logger.ErrorContext(baseCtx, "persist queued approval explanation timeout failed", "error", err)
			}
			return
		}

		started := time.Now()
		logger.InfoContext(baseCtx, "approval explanation started", "risk", approval.Risk, "queue_ms", started.Sub(queuedAt).Milliseconds())
		review, reviewErr := explainer.Review(explanationCtx, input)
		if errors.Is(explanationCtx.Err(), context.Canceled) {
			logger.InfoContext(baseCtx, "approval explanation canceled", "duration_ms", time.Since(started).Milliseconds())
			return
		}
		review = s.normalizeCommandReview(review, reviewErr, input.Policy.Risk, timeoutSeconds)
		persistCtx, cancelPersist := context.WithTimeout(baseCtx, 3*time.Second)
		err := s.persistPendingApprovalExplanation(persistCtx, approval, input.Policy.Risk, review, time.Since(started))
		cancelPersist()
		if err != nil {
			current, getErr := s.store.GetApproval(baseCtx, approval.ID)
			if getErr == nil && current.Status != "pending" {
				logger.InfoContext(baseCtx, "approval explanation discarded after decision", "status", current.Status, "duration_ms", time.Since(started).Milliseconds())
				return
			}
			logger.ErrorContext(baseCtx, "persist approval explanation failed", "error", err, "duration_ms", time.Since(started).Milliseconds())
			return
		}
		logger.InfoContext(baseCtx, "approval explanation completed", "status", review.Status, "duration_ms", time.Since(started).Milliseconds())
	}()
}

func (s *Service) persistPendingApprovalExplanation(ctx context.Context, approval domain.Approval, risk domain.RiskLevel, review domain.CommandReview, duration time.Duration) error {
	reviewJSON, err := json.Marshal(review)
	if err != nil {
		return fmt.Errorf("encode approval explanation: %w", err)
	}
	if err := s.store.UpdatePendingApprovalExplanation(ctx, approval.ID, approval.RunID, string(reviewJSON)); err != nil {
		return err
	}
	s.audit(ctx, approval.RunID, "command_ai_explained", "command-explainer-agent", map[string]any{
		"approval_id": approval.ID, "status": review.Status, "deterministic_risk": risk,
		"model": review.Model, "duration_ms": duration.Milliseconds(),
	})
	notifyApproval(ctx, domain.ExecResult{
		RunID: approval.RunID, Status: "approval_required", Risk: approval.Risk,
		ApprovalID: approval.ID,
	})
	return nil
}

func (s *Service) normalizeCommandReview(review domain.CommandReview, reviewErr error, deterministicRisk domain.RiskLevel, timeoutSeconds int) domain.CommandReview {
	if reviewErr != nil {
		model := review.Model
		message := reviewErr.Error()
		if errors.Is(reviewErr, context.DeadlineExceeded) || strings.Contains(strings.ToLower(message), "context deadline exceeded") {
			message = fmt.Sprintf("command explanation model did not respond within %d seconds", effectiveSubagentTimeoutSeconds(timeoutSeconds))
		}
		review = domain.CommandReview{
			Status: "unavailable", Model: model, DeterministicRisk: deterministicRisk,
			Errors: []string{message}, ReviewedAt: time.Now().UTC(),
		}
	}
	if review.Status != "completed" && review.Status != "degraded" && review.Status != "unavailable" {
		review.Status = "degraded"
	}
	if review.ReviewedAt.IsZero() {
		review.ReviewedAt = time.Now().UTC()
	}
	review.DeterministicRisk = deterministicRisk
	if len(review.Errors) > 5 {
		review.Errors = review.Errors[:5]
	}
	for index := range review.Errors {
		review.Errors[index] = s.redactor.Redact(review.Errors[index])
		if len(review.Errors[index]) > 800 {
			review.Errors[index] = review.Errors[index][:800]
		}
	}
	return review
}

func effectiveSubagentTimeoutSeconds(timeoutSeconds int) int {
	if timeoutSeconds < domain.MinSubagentTimeoutSeconds || timeoutSeconds > domain.MaxSubagentTimeoutSeconds {
		return domain.DefaultSubagentTimeoutSeconds
	}
	return timeoutSeconds
}

func (s *Service) Approve(ctx context.Context, approvalID, reason, actor string) (domain.ExecResult, error) {
	return s.ApproveWithScope(ctx, approvalID, reason, "once", actor)
}

func (s *Service) ApproveWithScope(ctx context.Context, approvalID, reason, scope, actor string) (domain.ExecResult, error) {
	logger := observability.FromContext(ctx).With("component", "approval", "approval_id", approvalID, "actor", actor)
	if scope == "" {
		scope = "once"
	}
	if scope != "once" && scope != "session" {
		logger.WarnContext(ctx, "approval rejected by validation", "scope", scope)
		return domain.ExecResult{}, fmt.Errorf("invalid approval scope %q", scope)
	}
	approval, err := s.store.GetApproval(ctx, approvalID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	if approval.Status != "pending" {
		logger.WarnContext(ctx, "approval decision ignored", "status", approval.Status)
		return domain.ExecResult{}, fmt.Errorf("approval is %s", approval.Status)
	}
	if time.Now().UTC().After(approval.ExpiresAt) {
		_ = s.store.DecideApproval(ctx, approval.ID, "expired", "approval expired")
		s.cancelApprovalExplanation(ctx, approval.ID, approval.RunID)
		logger.WarnContext(ctx, "approval expired before decision", "run_id", approval.RunID)
		return domain.ExecResult{}, fmt.Errorf("approval expired")
	}
	if approval.Risk == domain.RiskCritical {
		if scope == "session" {
			return domain.ExecResult{}, fmt.Errorf("critical operations cannot be approved for an entire session")
		}
		if strings.TrimSpace(reason) == "" {
			return domain.ExecResult{}, fmt.Errorf("approval reason is required for critical operations")
		}
	}
	requestData, err := s.encryptor.Decrypt(approval.RequestCipher)
	if err != nil {
		return domain.ExecResult{}, err
	}
	if len(requestData) == 0 {
		requestData = []byte(approval.RequestJSON)
	}
	var req domain.ExecRequest
	if err := json.Unmarshal(requestData, &req); err != nil {
		return domain.ExecResult{}, err
	}
	_, digest, err := canonicalRequest(req)
	if err != nil || digest != approval.RequestDigest {
		return domain.ExecResult{}, fmt.Errorf("approved request digest no longer matches")
	}
	if scope == "session" && requiresOneTimeApproval(req) {
		return domain.ExecResult{}, fmt.Errorf("this operation requires a fresh one-time approval for every invocation")
	}
	run, err := s.store.GetRun(ctx, approval.RunID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	host, err := s.store.GetHost(ctx, approval.HostID)
	if err != nil {
		return domain.ExecResult{}, err
	}
	if scope == "session" {
		if approval.SessionID == "" {
			return domain.ExecResult{}, fmt.Errorf("approval has no Agent session and cannot create a session grant")
		}
		fingerprint, err := approvalFingerprint(req)
		if err != nil {
			return domain.ExecResult{}, err
		}
		if err := s.store.DecideApprovalWithSessionGrant(ctx, approval.ID, reason, approval.SessionID, fingerprint, time.Now().UTC().Add(8*time.Hour), approval.Risk); err != nil {
			return domain.ExecResult{}, err
		}
	} else if err := s.store.ApprovePending(ctx, approval.ID, reason, approval.Risk); err != nil {
		return domain.ExecResult{}, err
	}
	s.cancelApprovalExplanation(ctx, approval.ID, approval.RunID)
	run.Status = "running"
	if err := s.store.UpdateRun(ctx, run); err != nil {
		return domain.ExecResult{}, err
	}
	s.audit(ctx, run.ID, "approval_granted", actor, map[string]any{"approval_id": approval.ID, "reason": reason, "scope": scope, "session_id": approval.SessionID})
	logger.InfoContext(ctx, "approval granted", "run_id", run.ID, "scope", scope, "risk", approval.Risk, "session_id", approval.SessionID)
	return s.execute(ctx, host, req, run, actor, nil, nil)
}

func (s *Service) Reject(ctx context.Context, approvalID, reason, actor string) error {
	logger := observability.FromContext(ctx).With("component", "approval", "approval_id", approvalID, "actor", actor)
	approval, err := s.store.GetApproval(ctx, approvalID)
	if err != nil {
		return err
	}
	if err := s.store.DecideApproval(ctx, approval.ID, "rejected", reason); err != nil {
		return err
	}
	s.cancelApprovalExplanation(ctx, approval.ID, approval.RunID)
	run, err := s.store.GetRun(ctx, approval.RunID)
	if err != nil {
		return err
	}
	run.Status = "rejected"
	run.Error = reason
	run.CompletedAt = time.Now().UTC()
	if err := s.store.UpdateRun(ctx, run); err != nil {
		return err
	}
	s.audit(ctx, run.ID, "approval_rejected", actor, map[string]any{"approval_id": approval.ID, "reason": reason})
	logger.InfoContext(ctx, "approval rejected", "run_id", run.ID, "session_id", approval.SessionID)
	return nil
}

func (s *Service) RejectPendingApprovalsForSession(ctx context.Context, sessionID, reason, actor string) (int, error) {
	approvals, err := s.store.ListPendingApprovalsForSession(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	rejected := 0
	for _, approval := range approvals {
		if err := s.Reject(ctx, approval.ID, reason, actor); err != nil {
			current, getErr := s.store.GetApproval(ctx, approval.ID)
			if getErr == nil && current.Status != "pending" {
				continue
			}
			return rejected, err
		}
		rejected++
	}
	return rejected, nil
}

// awaitApproval keeps an Agent Tool call suspended until its exact approval is
// decided and, when approved, until the approved execution finishes. Decisions
// remain durable in SQLite; polling also makes this work when the approval HTTP
// request is handled by a different goroutine.
func (s *Service) awaitApproval(ctx context.Context, initial domain.ExecResult) (domain.ExecResult, error) {
	logger := observability.FromContext(ctx).With("component", "approval", "approval_id", initial.ApprovalID, "run_id", initial.RunID)
	logger.DebugContext(ctx, "agent tool call paused for approval")
	poll := time.NewTicker(250 * time.Millisecond)
	heartbeat := time.NewTicker(15 * time.Second)
	defer poll.Stop()
	defer heartbeat.Stop()

	for {
		approval, err := s.store.GetApproval(ctx, initial.ApprovalID)
		if err != nil {
			return domain.ExecResult{}, err
		}
		run, err := s.store.GetRun(ctx, approval.RunID)
		if err != nil {
			return domain.ExecResult{}, err
		}

		if approval.Status == "pending" && time.Now().UTC().After(approval.ExpiresAt) {
			if err := s.store.DecideApproval(ctx, approval.ID, "expired", "approval expired"); err == nil {
				s.cancelApprovalExplanation(ctx, approval.ID, approval.RunID)
				run.Status = "expired"
				run.Error = "approval expired"
				run.CompletedAt = time.Now().UTC()
				_ = s.store.UpdateRun(ctx, run)
				s.audit(ctx, run.ID, "approval_expired", "control-plane", map[string]any{"approval_id": approval.ID})
			}
			continue
		}

		switch approval.Status {
		case "rejected", "expired":
			logger.InfoContext(ctx, "agent tool approval wait finished", "status", approval.Status)
			result := execResultFromRun(run, approval.ID, approval.Reason)
			notifyApproval(ctx, result)
			return result, nil
		case "approved":
			if run.Status != "created" && run.Status != "approval_required" && run.Status != "running" {
				logger.InfoContext(ctx, "agent tool approval wait finished", "status", run.Status)
				return execResultFromRun(run, approval.ID, ""), nil
			}
		}

		select {
		case <-ctx.Done():
			logger.WarnContext(ctx, "agent tool approval wait canceled", "error", ctx.Err())
			return domain.ExecResult{}, ctx.Err()
		case <-poll.C:
		case <-heartbeat.C:
			notifyApproval(ctx, initial)
		}
	}
}

func execResultFromRun(run domain.Run, approvalID, operatorInstruction string) domain.ExecResult {
	stderr := run.StderrRedacted
	if stderr == "" && run.Error != "" {
		stderr = run.Error
	}
	duration := time.Duration(0)
	if !run.CompletedAt.IsZero() {
		duration = run.CompletedAt.Sub(run.StartedAt)
	}
	return domain.ExecResult{
		RunID: run.ID, Status: run.Status, Risk: run.Risk, ApprovalID: approvalID,
		OperatorInstruction: operatorInstruction, ExitCode: run.ExitCode,
		Stdout: run.StdoutRedacted, Stderr: stderr,
		Duration: duration, CompletedAt: run.CompletedAt,
	}
}

func (s *Service) execute(ctx context.Context, host domain.Host, req domain.ExecRequest, run domain.Run, actor string, hits []string, stream func(string, []byte)) (domain.ExecResult, error) {
	logger := observability.FromContext(ctx).With(
		"component", "ssh", "run_id", run.ID, "session_id", run.SessionID, "host_id", host.ID,
		"mode", req.Mode, "program", req.Program, "elevated", req.Elevated, "risk", run.Risk,
	)
	logger.InfoContext(ctx, "operation execution started")
	if req.Mode == domain.ExecWorkspaceUpload {
		prepared, prepareErr := s.prepareWorkspaceUpload(req)
		if prepareErr != nil {
			run.Status = "failed"
			run.Error = prepareErr.Error()
			run.CompletedAt = time.Now().UTC()
			_ = s.store.UpdateRun(ctx, run)
			s.audit(ctx, run.ID, "command_failed", actor, map[string]any{"error": prepareErr.Error()})
			logger.ErrorContext(ctx, "Workspace upload source validation failed", "error", prepareErr)
			return domain.ExecResult{RunID: run.ID, Status: run.Status, Risk: run.Risk, Stderr: prepareErr.Error(), CompletedAt: run.CompletedAt}, prepareErr
		}
		req = prepared
	}
	approvedReq := req
	transportReq := req
	if req.Mode == domain.ExecRemoteRead {
		transportReq.Mode = domain.ExecScript
		transportReq.Script = buildRemoteFileReadScript(req)
	}
	if req.Mode == domain.ExecRemoteSearch {
		transportReq.Mode = domain.ExecScript
		transportReq.Script = buildRemoteFileSearchScript(req)
	}
	if req.Mode == domain.ExecRemoteEdit {
		prepared, prepareErr := s.prepareRemoteFileChange(req)
		if prepareErr != nil {
			run.Status = "failed"
			run.Error = prepareErr.Error()
			run.CompletedAt = time.Now().UTC()
			_ = s.store.UpdateRun(ctx, run)
			s.audit(ctx, run.ID, "command_failed", actor, map[string]any{"error": prepareErr.Error()})
			logger.ErrorContext(ctx, "remote file change preparation failed", "error", prepareErr)
			return domain.ExecResult{RunID: run.ID, Status: run.Status, Risk: run.Risk, Stderr: prepareErr.Error(), Change: req.Change, CompletedAt: run.CompletedAt}, prepareErr
		}
		transportReq = prepared
	}
	hostIDs := []string{host.ID}
	if req.Mode == domain.ExecSSHFileTransfer {
		hostIDs = append(hostIDs, req.SourceHostID)
	}
	release, err := s.acquire(ctx, hostIDs...)
	if err != nil {
		logger.WarnContext(ctx, "SSH execution canceled before acquiring capacity", "error", err)
		return domain.ExecResult{}, err
	}
	defer release()
	var connection sshx.ConnectionSpec
	if !isWorkspaceMode(req.Mode) && req.Mode != domain.ExecSSHFileTransfer {
		latestHost, connectionErr := s.store.GetHost(ctx, host.ID)
		if connectionErr == nil {
			var currentDigest string
			connection, currentDigest, connectionErr = s.resolveSSHConnection(ctx, latestHost)
			if connectionErr == nil {
				connectionErr = verifySSHRequestBinding(req, currentDigest)
			}
		}
		if connectionErr == nil {
			connection, connectionErr = s.hydrateSSHConnection(connection, req.Elevated)
		}
		err = connectionErr
	}
	if err != nil {
		run.Status = "failed"
		run.Error = err.Error()
		run.CompletedAt = time.Now().UTC()
		_ = s.store.UpdateRun(ctx, run)
		s.audit(ctx, run.ID, "command_failed", actor, map[string]any{"error": err.Error()})
		logger.ErrorContext(ctx, "SSH credential preparation failed", "error", err)
		return domain.ExecResult{RunID: run.ID, Status: run.Status, Risk: run.Risk, CompletedAt: run.CompletedAt}, err
	}
	s.audit(ctx, run.ID, "command_started", actor, map[string]any{"risk": run.Risk, "digest": run.RequestDigest})
	var raw sshx.RawResult
	var execErr error
	if req.Mode == domain.ExecSSHFileTransfer {
		raw, execErr = s.executeSSHFileTransfer(ctx, req)
	} else if isWorkspaceMode(req.Mode) {
		raw, execErr = s.executeWorkspace(ctx, req)
	} else if streaming, ok := s.transport.(sshx.StreamingTransport); ok && stream != nil {
		raw, execErr = streaming.ExecStream(ctx, connection, transportReq, stream)
	} else {
		raw, execErr = s.transport.Exec(ctx, connection, transportReq)
	}
	run.ExitCode = raw.ExitCode
	run.StdoutRedacted = s.redactor.Redact(string(raw.Stdout))
	run.StderrRedacted = s.redactor.Redact(string(raw.Stderr))
	run.StdoutCipher, _ = s.encryptor.Encrypt(raw.Stdout)
	run.StderrCipher, _ = s.encryptor.Encrypt(raw.Stderr)
	run.CompletedAt = time.Now().UTC()
	if execErr != nil {
		run.Status = "failed"
		run.Error = execErr.Error()
	} else if raw.ExitCode != 0 {
		run.Status = "failed"
		run.Error = "remote command exited with code " + strconv.Itoa(raw.ExitCode)
	} else {
		run.Status = "completed"
	}
	if err := s.store.UpdateRun(ctx, run); err != nil {
		logger.ErrorContext(ctx, "persist SSH execution result failed", "error", err)
		return domain.ExecResult{}, err
	}
	s.audit(ctx, run.ID, "command_completed", actor, map[string]any{"status": run.Status, "exit_code": run.ExitCode, "duration_ms": raw.Duration.Milliseconds()})
	completion := logger.InfoContext
	if run.Status == "failed" {
		completion = logger.ErrorContext
	}
	completion(ctx, "SSH execution completed", "status", run.Status, "exit_code", run.ExitCode, "duration_ms", raw.Duration.Milliseconds(), "stdout_bytes", len(raw.Stdout), "stderr_bytes", len(raw.Stderr), "error", execErr)
	result := domain.ExecResult{
		RunID: run.ID, Status: run.Status, Risk: run.Risk, ExitCode: run.ExitCode,
		Stdout: run.StdoutRedacted, Stderr: run.StderrRedacted,
		Duration: raw.Duration, PolicyHits: hits, Change: approvedReq.Change, CompletedAt: run.CompletedAt,
	}
	if run.Status == "completed" && (approvedReq.Mode == domain.ExecRemoteSearch || approvedReq.Mode == domain.ExecWorkspaceSearch) {
		decorateFileSearchResult(&result, approvedReq.SearchPattern, approvedReq.SearchMatchMode, approvedReq.ContextLines)
	}
	if approvedReq.Change != nil {
		path := approvedReq.RemotePath
		if approvedReq.Mode == domain.ExecWorkspaceEdit {
			path = approvedReq.RelativePath
		}
		metadata := parseFileEditOutput(path, approvedReq.Validator, result.Stdout)
		result.File = &metadata
	}
	return result, execErr
}

func (s *Service) hydrateHostSecrets(host domain.Host, includeSudo bool) (domain.Host, error) {
	if host.AuthType == "password" {
		plain, err := s.encryptor.Decrypt(host.PasswordCipher)
		if err != nil {
			return domain.Host{}, fmt.Errorf("decrypt SSH password: %w", err)
		}
		host.Password = string(plain)
	}
	if host.AuthType == "key" && host.PrivateKeyCipher != "" {
		plain, err := s.encryptor.Decrypt(host.PrivateKeyCipher)
		if err != nil {
			return domain.Host{}, fmt.Errorf("decrypt SSH private key: %w", err)
		}
		host.PrivateKey = plain
	}
	if host.ProxyPasswordCipher != "" {
		plain, err := s.encryptor.Decrypt(host.ProxyPasswordCipher)
		if err != nil {
			return domain.Host{}, fmt.Errorf("decrypt SSH proxy password: %w", err)
		}
		host.ProxyPassword = string(plain)
	}
	if includeSudo && host.SudoMode == "password" {
		plain, err := s.encryptor.Decrypt(host.SudoCipher)
		if err != nil {
			return domain.Host{}, fmt.Errorf("decrypt sudo password: %w", err)
		}
		host.SudoPassword = string(plain)
	}
	return host, nil
}

func validateExecutionRequest(host domain.Host, req domain.ExecRequest) error {
	if isWorkspaceMode(req.Mode) {
		if host.AuthType != "workspace" || req.Elevated {
			return fmt.Errorf("invalid workspace execution target")
		}
		if req.Mode == domain.ExecWorkspaceSearch {
			if req.WorkspaceID == "" || req.RelativePath == "" {
				return fmt.Errorf("workspace file search requires a workspace, path, and pattern")
			}
			if err := validateFileSearchInput(req.SearchPattern, req.SearchMatchMode, req.ContextLines); err != nil {
				return fmt.Errorf("invalid Workspace file search: %w", err)
			}
		}
		return nil
	}
	switch req.Mode {
	case domain.ExecRemoteRead:
		if req.RemotePath == "" {
			return fmt.Errorf("remote file read requires an absolute path")
		}
		if req.MetadataOnly && (req.MaxBytes != 0 || req.OffsetBytes != 0 || req.TailLines != 0) {
			return fmt.Errorf("metadata_only cannot be combined with max_bytes, offset_bytes, or tail_lines")
		}
		if req.MaxBytes < 0 || req.TailLines < 0 || (req.OffsetBytes != 0 && req.TailLines != 0) {
			return fmt.Errorf("invalid remote file read range")
		}
	case domain.ExecRemoteSearch:
		if req.RemotePath == "" || req.SearchPattern == "" {
			return fmt.Errorf("remote file search requires an absolute path and pattern")
		}
		if err := validateFileSearchInput(req.SearchPattern, req.SearchMatchMode, req.ContextLines); err != nil {
			return fmt.Errorf("invalid remote file search: %w", err)
		}
	}
	if req.Mode == domain.ExecSSHFileTransfer && req.Elevated {
		return fmt.Errorf("elevated mode is not supported for SFTP transfers")
	}
	usesSudo, err := policy.ContainsProgram(req, "sudo")
	if err == nil && usesSudo {
		return fmt.Errorf("do not invoke sudo directly; set elevated=true and provide the underlying program")
	}
	if !req.Elevated {
		return nil
	}
	if req.Mode == domain.ExecWorkspaceUpload || req.Mode == domain.ExecSSHFileTransfer {
		return fmt.Errorf("elevated mode is not supported for SFTP transfers")
	}
	if host.SudoMode == "none" || host.SudoMode == "" {
		return fmt.Errorf("host %q does not allow managed sudo; edit the host sudo mode first", host.Name)
	}
	if host.SudoMode == "password" && host.SudoCipher == "" {
		return fmt.Errorf("host %q has no encrypted sudo password", host.Name)
	}
	return nil
}

func validateRequestLimits(req domain.ExecRequest, limits config.Limits, redactor *security.Redactor) error {
	if req.Mode == domain.ExecWorkspaceShell {
		switch req.WorkspaceShellBackend {
		case domain.WorkspaceShellModeSandbox, domain.WorkspaceShellModeHost:
		default:
			return fmt.Errorf("workspace_shell_backend must be sandbox or host")
		}
	} else if req.WorkspaceShellBackend != "" {
		return fmt.Errorf("workspace_shell_backend is only valid for workspace shell requests")
	}
	changeTooLarge := false
	if req.Change != nil {
		changeTooLarge = len(req.Change.Diff) > 2<<20
	}
	if len(req.Program) > 512 || len(req.Args) > 128 || len(req.Env) > 64 || len(req.Script) > 1<<20 || changeTooLarge {
		return fmt.Errorf("execution request exceeds program, argument, environment, or 1 MiB content limits")
	}
	for _, argument := range req.Args {
		if len(argument) > 32<<10 || strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("invalid command argument")
		}
	}
	if req.Cwd != "" {
		if req.Mode == domain.ExecWorkspaceShell {
			if filepath.IsAbs(req.Cwd) || filepath.Clean(req.Cwd) != req.Cwd || strings.ContainsAny(req.Cwd, "\x00\r\n") {
				return fmt.Errorf("workspace shell cwd must be a clean relative path")
			}
		} else if !posixpath.IsAbs(req.Cwd) || posixpath.Clean(req.Cwd) != req.Cwd || strings.ContainsAny(req.Cwd, "\x00\r\n") {
			return fmt.Errorf("cwd must be a clean absolute remote path")
		}
	}
	if req.RemotePath != "" && (!posixpath.IsAbs(req.RemotePath) || posixpath.Clean(req.RemotePath) != req.RemotePath || strings.ContainsAny(req.RemotePath, "\x00\r\n")) {
		return fmt.Errorf("remote_path must be a clean absolute path")
	}
	if req.SourcePath != "" && (!posixpath.IsAbs(req.SourcePath) || posixpath.Clean(req.SourcePath) != req.SourcePath || strings.ContainsAny(req.SourcePath, "\x00\r\n")) {
		return fmt.Errorf("source_path must be a clean absolute path")
	}
	for key, value := range req.Env {
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`).MatchString(key) || len(value) > 32<<10 || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("invalid environment variable %q", key)
		}
		if redactor != nil && redactor.Redact(key+"="+value) != key+"="+value {
			return fmt.Errorf("environment variable %q appears to contain a secret; credentials must be managed by the control plane", key)
		}
	}
	if req.Mode != domain.ExecProgram {
		return nil
	}
	program := strings.ToLower(posixpath.Base(req.Program))
	interactive := map[string]bool{"bash": true, "sh": true, "zsh": true, "fish": true, "su": true, "vi": true, "vim": true, "nano": true, "emacs": true, "less": true, "more": true, "man": true}
	if interactive[program] {
		return fmt.Errorf("interactive program %q is unsupported because SSH tools do not allocate a PTY; use a non-interactive command or ssh_run_script", program)
	}
	if program == "systemctl" && len(req.Args) > 0 && req.Args[0] == "edit" {
		return fmt.Errorf("interactive systemctl edit is unsupported; use ssh_file_edit on the unit or override file")
	}
	if packageMutation(req.Args) {
		requiredFlag := ""
		switch program {
		case "apt", "apt-get":
			requiredFlag = "-y or --assume-yes"
			if hasAnyArg(req.Args, "-y", "--yes", "--assume-yes") {
				return nil
			}
		case "dnf", "yum":
			requiredFlag = "-y or --assumeyes"
			if hasAnyArg(req.Args, "-y", "--assumeyes") {
				return nil
			}
		case "pacman":
			requiredFlag = "--noconfirm"
			if hasAnyArg(req.Args, "--noconfirm") {
				return nil
			}
		default:
			return nil
		}
		return fmt.Errorf("package operation may wait for interactive input; add %s and keep the exact package list in args", requiredFlag)
	}
	_ = limits
	return nil
}

func isHostWorkspaceShell(req domain.ExecRequest) bool {
	return req.Mode == domain.ExecWorkspaceShell && req.WorkspaceShellBackend == domain.WorkspaceShellModeHost
}

func requiresOneTimeApproval(req domain.ExecRequest) bool {
	if isHostWorkspaceShell(req) {
		return true
	}
	switch req.Mode {
	case domain.ExecRemoteRead, domain.ExecRemoteSearch, domain.ExecWorkspaceRead, domain.ExecWorkspaceSearch:
		return true
	}
	for _, program := range []string{"cat", "cut", "grep", "head", "less", "more", "tail"} {
		if found, err := policy.ContainsProgram(req, program); err == nil && found {
			return true
		}
	}
	return false
}

func packageMutation(args []string) bool {
	for _, argument := range args {
		switch strings.ToLower(argument) {
		case "install", "remove", "upgrade", "full-upgrade", "dist-upgrade", "-s", "-r", "-u", "-sy", "-syu":
			return true
		}
	}
	return false
}

func hasAnyArg(args []string, candidates ...string) bool {
	for _, argument := range args {
		for _, candidate := range candidates {
			if argument == candidate {
				return true
			}
		}
	}
	return false
}

func containsCredentialControl(value string) bool {
	return strings.ContainsAny(value, "\x00\r\n")
}

func (s *Service) StartTask(ctx context.Context, req domain.ExecRequest, actor string) (domain.Task, error) {
	if blockingApprovalsFromContext(ctx) {
		decision, err := s.Evaluate(ctx, req)
		if err != nil {
			return domain.Task{}, err
		}
		if decision.Action == domain.ActionApprove || decision.Action == domain.ActionBreakGlass {
			task := domain.Task{ID: ids.New("task"), HostID: req.HostID, Status: "waiting_for_approval", StartedAt: time.Now().UTC()}
			result, submitErr := s.Submit(ctx, req, actor)
			task.RunID = result.RunID
			task.Status = result.Status
			task.OperatorInstruction = result.OperatorInstruction
			task.EndedAt = time.Now().UTC()
			state := &taskState{task: task, result: result}
			if submitErr != nil {
				state.err = submitErr.Error()
			}
			s.taskMu.Lock()
			s.tasks[task.ID] = state
			s.taskMu.Unlock()
			_ = s.store.UpsertTask(context.Background(), task, result, state.err)
			return task, submitErr
		}
	}

	background := context.Background()
	if sessionID := SessionIDFromContext(ctx); sessionID != "" {
		background = WithSessionID(background, sessionID)
	}
	taskCtx, cancel := context.WithCancel(background)
	task := domain.Task{ID: ids.New("task"), HostID: req.HostID, Status: "running", StartedAt: time.Now().UTC()}
	state := &taskState{task: task, result: domain.ExecResult{Status: "running"}, cancel: cancel}
	s.taskMu.Lock()
	s.tasks[task.ID] = state
	s.taskMu.Unlock()
	if err := s.store.UpsertTask(context.Background(), task, state.result, ""); err != nil {
		cancel()
		return domain.Task{}, err
	}
	go func() {
		result, err := s.submit(taskCtx, req, actor, func(streamName string, data []byte) {
			s.taskMu.Lock()
			defer s.taskMu.Unlock()
			chunk := s.redactor.Redact(string(data))
			if streamName == "stderr" {
				state.result.Stderr += chunk
			} else {
				state.result.Stdout += chunk
			}
			_ = s.store.UpsertTask(context.Background(), state.task, state.result, state.err)
		})
		s.taskMu.Lock()
		defer s.taskMu.Unlock()
		state.result = result
		state.task.RunID = result.RunID
		state.task.EndedAt = time.Now().UTC()
		if state.task.Status == "cancelled" {
			delete(s.tasks, state.task.ID)
			return
		}
		state.task.Status = result.Status
		if err != nil {
			state.err = err.Error()
			state.task.Status = "failed"
		}
		_ = s.store.UpsertTask(context.Background(), state.task, state.result, state.err)
		delete(s.tasks, state.task.ID)
	}()
	_ = ctx
	return task, nil
}

func (s *Service) GetTask(id string) (domain.Task, domain.ExecResult, string, error) {
	s.taskMu.RLock()
	state, ok := s.tasks[id]
	if ok {
		task, result, taskErr := state.task, state.result, state.err
		s.taskMu.RUnlock()
		return task, result, taskErr, nil
	}
	s.taskMu.RUnlock()
	return s.store.GetTask(context.Background(), id)
}

func (s *Service) CancelTask(id, actor string) error {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	state, ok := s.tasks[id]
	if !ok {
		if _, _, _, err := s.store.GetTask(context.Background(), id); err != nil {
			return err
		}
		return fmt.Errorf("task is not running and cannot be cancelled")
	}
	if state.task.Status != "running" && state.task.Status != "waiting_for_approval" && state.task.Status != "approval_required" {
		return fmt.Errorf("task is not running and cannot be cancelled")
	}
	if state.cancel != nil {
		state.cancel()
	}
	state.task.Status = "cancelled"
	state.task.EndedAt = time.Now().UTC()
	s.audit(context.Background(), state.task.RunID, "task_cancelled", actor, map[string]any{"task_id": id})
	_ = s.store.UpsertTask(context.Background(), state.task, state.result, state.err)
	return nil
}

func (s *Service) ReadFile(ctx context.Context, hostID, path string, maxBytes int, actor string) (domain.ExecResult, error) {
	return s.ReadFileAdvanced(ctx, hostID, path, false, maxBytes, 0, 0, false, actor)
}

func (s *Service) ListFiles(ctx context.Context, hostID, path string, actor string) (domain.ExecResult, error) {
	if !posixpath.IsAbs(path) {
		return domain.ExecResult{}, fmt.Errorf("remote directory path must be absolute")
	}
	return s.Submit(ctx, domain.ExecRequest{HostID: hostID, Mode: domain.ExecProgram, Program: "ls", Args: []string{"-la", "--", path}, Reason: "list a remote directory for diagnosis"}, actor)
}

func (s *Service) GetRun(ctx context.Context, id string, includeRaw bool) (HistoryResult, error) {
	run, err := s.store.GetRun(ctx, id)
	if err != nil {
		return HistoryResult{}, err
	}
	result := HistoryResult{Run: run}
	if includeRaw {
		stdout, err := s.encryptor.Decrypt(run.StdoutCipher)
		if err != nil {
			return HistoryResult{}, err
		}
		stderr, err := s.encryptor.Decrypt(run.StderrCipher)
		if err != nil {
			return HistoryResult{}, err
		}
		result.StdoutRaw = string(stdout)
		result.StderrRaw = string(stderr)
	}
	return result, nil
}

func (s *Service) SearchRuns(ctx context.Context, query, hostID string, limit int) ([]domain.Run, error) {
	return s.store.SearchRuns(ctx, query, hostID, limit)
}

// RetryApprovalExplanation reruns the tool-free command explainer for an
// existing pending approval. It never changes risk, decides the approval, or
// executes the operation.
func (s *Service) RetryApprovalExplanation(ctx context.Context, approvalID, actor string) (domain.Approval, error) {
	logger := observability.FromContext(ctx).With("component", "approval", "approval_id", approvalID, "actor", actor)
	approval, err := s.store.GetApproval(ctx, approvalID)
	if err != nil {
		return domain.Approval{}, err
	}
	if approval.Status != "pending" {
		return domain.Approval{}, fmt.Errorf("approval is %s", approval.Status)
	}
	if time.Now().UTC().After(approval.ExpiresAt) {
		return domain.Approval{}, fmt.Errorf("approval expired")
	}
	settings, err := s.store.GetSystemSettings(ctx)
	if err != nil {
		return domain.Approval{}, err
	}
	if !settings.ApprovalExplanationsEnabled {
		return domain.Approval{}, fmt.Errorf("approval explanations are disabled in system settings")
	}
	explainer := s.commandExplainer()
	if explainer == nil {
		return domain.Approval{}, fmt.Errorf("command explanation Agent is unavailable for the active model")
	}

	requestData, err := s.encryptor.Decrypt(approval.RequestCipher)
	if err != nil {
		return domain.Approval{}, err
	}
	if len(requestData) == 0 {
		requestData = []byte(approval.RequestJSON)
	}
	var req domain.ExecRequest
	if err := json.Unmarshal(requestData, &req); err != nil {
		return domain.Approval{}, err
	}
	_, digest, err := canonicalRequest(req)
	if err != nil || digest != approval.RequestDigest {
		return domain.Approval{}, fmt.Errorf("approval request digest no longer matches")
	}
	run, err := s.store.GetRun(ctx, approval.RunID)
	if err != nil {
		return domain.Approval{}, err
	}
	host, err := s.store.GetHost(ctx, approval.HostID)
	if err != nil {
		return domain.Approval{}, err
	}

	action := domain.ActionApprove
	if approval.Risk == domain.RiskCritical {
		action = domain.ActionBreakGlass
	}
	planStep := ""
	if approval.SessionID != "" {
		if plan, planErr := s.store.GetAgentPlan(ctx, approval.SessionID); planErr == nil {
			for _, step := range plan.Steps {
				if step.Status == "in_progress" {
					planStep = fmt.Sprintf("%d. %s", step.Number, step.Title)
					break
				}
			}
		}
	}
	input := domain.CommandReviewInput{
		Request:  req,
		Policy:   domain.Decision{Risk: approval.Risk, Action: action, Reason: "pending operation requires human approval"},
		Host:     domain.HostCapability{ID: host.ID, Name: host.Name, AuthType: host.AuthType, SudoMode: host.SudoMode},
		PlanStep: planStep, RequestDigest: digest,
	}

	retryCtx, cancelRetry := context.WithCancel(ctx)
	task := &approvalExplanationTask{cancel: cancelRetry}
	s.registerApprovalExplanation(approval.ID, task)
	defer cancelRetry()
	defer s.clearApprovalExplanation(approval.ID, task)

	// Close the decision race between the initial read and task registration.
	current, err := s.store.GetApproval(retryCtx, approval.ID)
	if err != nil {
		return domain.Approval{}, err
	}
	if current.Status != "pending" {
		return domain.Approval{}, fmt.Errorf("approval is %s", current.Status)
	}
	if err := s.store.UpdateRunAIReview(retryCtx, run.ID, ""); err != nil {
		return domain.Approval{}, err
	}

	logger.InfoContext(ctx, "approval explanation retry started", "run_id", run.ID, "risk", approval.Risk)
	started := time.Now()
	timeoutSeconds := effectiveSubagentTimeoutSeconds(settings.SubagentTimeoutSeconds)
	explanationCtx, cancel := context.WithTimeout(retryCtx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	select {
	case s.explanationSem <- struct{}{}:
		defer func() { <-s.explanationSem }()
	case <-explanationCtx.Done():
		return domain.Approval{}, explanationCtx.Err()
	}
	var review domain.CommandReview
	var reviewErr error
	if freshExplainer, ok := explainer.(FreshCommandExplainer); ok {
		review, reviewErr = freshExplainer.ReviewFresh(explanationCtx, input)
	} else {
		review, reviewErr = explainer.Review(explanationCtx, input)
	}
	cancel()
	if retryCtx.Err() != nil {
		return domain.Approval{}, retryCtx.Err()
	}
	review = s.normalizeCommandReview(review, reviewErr, approval.Risk, timeoutSeconds)
	reviewJSON, err := json.Marshal(review)
	if err != nil {
		return domain.Approval{}, err
	}
	if err := s.store.UpdatePendingApprovalExplanation(retryCtx, approval.ID, run.ID, string(reviewJSON)); err != nil {
		return domain.Approval{}, err
	}
	s.audit(ctx, run.ID, "command_ai_explanation_retried", actor, map[string]any{
		"approval_id": approval.ID, "status": review.Status, "deterministic_risk": approval.Risk,
		"model": review.Model, "duration_ms": time.Since(started).Milliseconds(),
	})
	logger.InfoContext(ctx, "approval explanation retry completed", "run_id", run.ID, "status", review.Status,
		"duration_ms", time.Since(started).Milliseconds())

	approval.RequestJSON = string(requestData)
	approval.AIReview = &review
	return approval, nil
}

func (s *Service) ListApprovals(ctx context.Context, status string, limit int) ([]domain.Approval, error) {
	approvals, err := s.store.ListApprovals(ctx, status, limit)
	if err != nil {
		return nil, err
	}
	for index := range approvals {
		plain, decryptErr := s.encryptor.Decrypt(approvals[index].RequestCipher)
		if decryptErr != nil {
			return nil, decryptErr
		}
		if len(plain) > 0 {
			approvals[index].RequestJSON = string(plain)
		}
		if run, runErr := s.store.GetRun(ctx, approvals[index].RunID); runErr == nil {
			approvals[index].AIReview = run.AIReview
		}
	}
	return approvals, nil
}

func (s *Service) ListAudit(ctx context.Context, runID string, limit int) ([]domain.AuditEvent, error) {
	return s.store.ListAudit(ctx, runID, limit)
}

func (s *Service) acquire(ctx context.Context, hostIDs ...string) (func(), error) {
	uniqueHostIDs := make([]string, 0, len(hostIDs))
	seen := make(map[string]struct{}, len(hostIDs))
	for _, hostID := range hostIDs {
		if _, exists := seen[hostID]; hostID == "" || exists {
			continue
		}
		seen[hostID] = struct{}{}
		uniqueHostIDs = append(uniqueHostIDs, hostID)
	}
	sort.Strings(uniqueHostIDs)
	s.semMu.Lock()
	hostSems := make([]chan struct{}, 0, len(uniqueHostIDs))
	for _, hostID := range uniqueHostIDs {
		hostSem := s.hostSems[hostID]
		if hostSem == nil {
			limit := s.limits.HostConcurrency
			if limit <= 0 {
				limit = 2
			}
			hostSem = make(chan struct{}, limit)
			s.hostSems[hostID] = hostSem
		}
		hostSems = append(hostSems, hostSem)
	}
	s.semMu.Unlock()
	select {
	case s.globalSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	acquired := make([]chan struct{}, 0, len(hostSems))
	for _, hostSem := range hostSems {
		select {
		case hostSem <- struct{}{}:
			acquired = append(acquired, hostSem)
		case <-ctx.Done():
			for index := len(acquired) - 1; index >= 0; index-- {
				<-acquired[index]
			}
			<-s.globalSem
			return nil, ctx.Err()
		}
	}
	return func() {
		for index := len(acquired) - 1; index >= 0; index-- {
			<-acquired[index]
		}
		<-s.globalSem
	}, nil
}

func (s *Service) audit(ctx context.Context, runID, eventType, actor string, data map[string]any) {
	if actor == "" {
		actor = "local-user"
	}
	_ = s.store.AppendAudit(ctx, domain.AuditEvent{RunID: runID, Type: eventType, Actor: actor, Data: data})
}

func normalizeRequest(req *domain.ExecRequest, limits config.Limits) {
	if req.Mode == "" {
		if req.Script != "" {
			req.Mode = domain.ExecScript
		} else {
			req.Mode = domain.ExecProgram
		}
	}
	if req.TimeoutSeconds <= 0 {
		req.TimeoutSeconds = limits.SyncTimeoutSeconds
	}
	if req.TimeoutSeconds > limits.MaxTimeoutSeconds {
		req.TimeoutSeconds = limits.MaxTimeoutSeconds
	}
	if req.Env == nil {
		req.Env = map[string]string{}
	}
}

func canonicalRequest(req domain.ExecRequest) (string, string, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256(data)
	return string(data), hex.EncodeToString(digest[:]), nil
}

func approvalFingerprint(req domain.ExecRequest) (string, error) {
	req.Reason = ""
	req.ExpectedChanges = ""
	req.Rollback = ""
	data, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
