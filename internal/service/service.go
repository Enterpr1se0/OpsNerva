package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
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
	"sync/atomic"
	"time"

	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/ids"
	"eino-ops-agent/internal/observability"
	"eino-ops-agent/internal/security"
	"eino-ops-agent/internal/skills"
	"eino-ops-agent/internal/sshx"
	"eino-ops-agent/internal/store"
	"golang.org/x/sync/singleflight"
)

type Service struct {
	store                *store.Store
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

	globalSem               chan struct{}
	semMu                   sync.Mutex
	hostSems                map[string]chan struct{}
	taskMu                  sync.RWMutex
	tasks                   map[string]*taskState
	reviewerMu              sync.RWMutex
	reviewer                ApprovalReviewer
	automaticReviewer       AutomaticApprovalReviewer
	explainWG               sync.WaitGroup
	explanationMu           sync.Mutex
	explanationActive       map[string]*approvalExplanationTask
	explanationSem          chan struct{}
	explanationSlots        chan struct{}
	automaticApprovalSem    chan struct{}
	mcpMu                   sync.RWMutex
	mcpRuntime              map[string]*mcpRuntimeState
	mcpSecretsMu            sync.Mutex
	mcpOAuthMu              sync.Mutex
	mcpOAuthFlows           map[string]*mcpOAuthFlow
	mcpOAuthByServer        map[string]*mcpOAuthFlow
	mcpActivityMu           sync.Mutex
	mcpActivitySubscribers  map[uint64]*mcpActivitySubscriber
	mcpActivitySubscriberID uint64
	mcpActivitySequence     atomic.Uint64
	modelMetadata           *modelMetadataCache
	executionCtx            context.Context
	executionCancel         context.CancelFunc
	executionMu             sync.Mutex
	executionClosed         bool
	executionCancels        map[string]context.CancelFunc
	cancelledExecutions     map[string]struct{}
	executionWG             sync.WaitGroup
	executionEventMu        sync.RWMutex
	executionSubscribers    map[string]map[uint64]*executionSubscriber
	executionOwners         map[string]executionOwner
	executionSubscriberID   uint64
	executionEventSequence  atomic.Uint64
	tunnelMu                sync.RWMutex
	tunnels                 map[string]*sshTunnelState
	shellMu                 sync.RWMutex
	shells                  map[string]*sshShellState
	webSem                  chan struct{}
	webRequests             singleflight.Group
}

const (
	maxConcurrentApprovalExplanations = 2
	maxQueuedApprovalExplanations     = 4
)

type approvalExplanationTask struct {
	cancel context.CancelFunc
}

type ApprovalReviewer interface {
	Review(context.Context, domain.CommandReviewInput) (domain.CommandReview, error)
}

type FreshApprovalReviewer interface {
	ReviewFresh(context.Context, domain.CommandReviewInput) (domain.CommandReview, error)
}

type AutomaticApprovalReviewer interface {
	Review(context.Context, domain.AutomaticApprovalInput) (domain.CommandReview, error)
}

type taskState struct {
	task       domain.Task
	result     domain.ExecResult
	err        string
	cancel     context.CancelFunc
	approvalID string
	sessionID  string
}

type HistoryResult struct {
	Run       domain.Run `json:"run"`
	StdoutRaw string     `json:"stdout_raw,omitempty"`
	StderrRaw string     `json:"stderr_raw,omitempty"`
}

func New(st *store.Store, transport sshx.Transport, encryptor *security.Encryptor, redactor *security.Redactor, limits config.Limits, runtimeConfig ...config.Config) *Service {
	global := limits.GlobalConcurrency
	if global <= 0 {
		global = 8
	}
	executionCtx, executionCancel := context.WithCancel(context.Background())
	result := &Service{
		store: st, transport: transport, encryptor: encryptor, redactor: redactor, limits: limits,
		workspaceSandboxPath: config.Default().WorkspaceSandboxPath,
		globalSem:            make(chan struct{}, global), hostSems: make(map[string]chan struct{}), tasks: make(map[string]*taskState), workspaces: make(map[string]config.Workspace), validators: make(map[string]config.Validator), mcpRuntime: make(map[string]*mcpRuntimeState),
		mcpOAuthFlows: make(map[string]*mcpOAuthFlow), mcpOAuthByServer: make(map[string]*mcpOAuthFlow),
		mcpActivitySubscribers: make(map[uint64]*mcpActivitySubscriber),
		modelMetadata:          newModelMetadataCache(modelsDevMetadataURL),
		explanationActive:      make(map[string]*approvalExplanationTask), explanationSem: make(chan struct{}, maxConcurrentApprovalExplanations), explanationSlots: make(chan struct{}, maxQueuedApprovalExplanations),
		automaticApprovalSem: make(chan struct{}, maxConcurrentApprovalExplanations),
		executionCtx:         executionCtx, executionCancel: executionCancel,
		executionCancels: make(map[string]context.CancelFunc), cancelledExecutions: make(map[string]struct{}),
		tunnels: make(map[string]*sshTunnelState),
		shells:  make(map[string]*sshShellState),
		webSem:  make(chan struct{}, 4),
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
	if err := s.store.InterruptActiveTasks(ctx); err != nil {
		return err
	}
	if err := s.store.InterruptActiveSSHShells(ctx); err != nil {
		return err
	}
	if err := s.store.FailPendingChatMessages(ctx); err != nil {
		return err
	}
	if err := s.recoverChatToolCalls(ctx); err != nil {
		return err
	}
	if err := s.store.InterruptRunningMCPToolCalls(ctx); err != nil {
		return err
	}
	_, err := s.store.PruneChatTurnsExcludedFromContext(ctx, "")
	return err
}

func (s *Service) recoverChatToolCalls(ctx context.Context) error {
	calls, err := s.store.ListRunningChatToolCalls(ctx)
	if err != nil {
		return err
	}
	for _, call := range calls {
		status := domain.ChatToolCallUnknown
		content := ""
		if call.RunID != "" {
			run, runErr := s.store.GetRun(ctx, call.RunID)
			if runErr == nil && run.Status == "approval_required" {
				continue
			}
			if runErr == nil {
				if terminal := persistedToolExecutionStatus(run.Status); terminal != "" {
					status = terminal
					if encoded, marshalErr := json.Marshal(execResultFromRun(run, "", "")); marshalErr == nil {
						content = string(encoded)
					}
				}
			}
		}
		if _, err := s.store.FinishChatToolCall(ctx, call.SessionID, call.ToolCallID, call.RunID, status, content, ""); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Store() *store.Store { return s.store }

func (s *Service) Shutdown(ctx context.Context) error {
	s.executionMu.Lock()
	if !s.executionClosed {
		s.executionClosed = true
		s.executionCancel()
	}
	s.executionMu.Unlock()

	done := make(chan struct{})
	go func() {
		s.executionWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

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

func (s *Service) GetChatContextSummary(ctx context.Context, sessionID string) (domain.ChatContextSummary, error) {
	return s.store.GetChatContextSummary(ctx, strings.TrimSpace(sessionID))
}

const maxChatSessionTitleRunes = 80

func normalizeChatSessionTitle(title string) (string, error) {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return "", fmt.Errorf("conversation title is required")
	}
	if len([]rune(title)) > maxChatSessionTitleRunes {
		return "", fmt.Errorf("conversation title must not exceed %d characters", maxChatSessionTitleRunes)
	}
	return title, nil
}

func (s *Service) RenameChatSession(ctx context.Context, sessionID, title, actor string) (domain.ChatSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return domain.ChatSession{}, fmt.Errorf("session id is required")
	}
	title, err := normalizeChatSessionTitle(title)
	if err != nil {
		return domain.ChatSession{}, err
	}
	current, err := s.store.GetChatSession(ctx, sessionID)
	if err != nil {
		return domain.ChatSession{}, err
	}
	if current.TitleSet && current.Title == title {
		return current, nil
	}
	session, err := s.store.SetChatSessionTitle(ctx, sessionID, title)
	if err != nil {
		return domain.ChatSession{}, err
	}
	s.audit(ctx, "", "chat_session_renamed", actor, map[string]any{"session_id": sessionID, "title": title})
	return session, nil
}

func (s *Service) SetGeneratedChatSessionTitle(ctx context.Context, sessionID, title string) (domain.ChatSession, bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return domain.ChatSession{}, false, fmt.Errorf("session id is required")
	}
	title, err := normalizeChatSessionTitle(title)
	if err != nil {
		return domain.ChatSession{}, false, err
	}
	session, changed, err := s.store.SetChatSessionTitleIfEmpty(ctx, sessionID, title)
	if err != nil || !changed {
		return session, changed, err
	}
	s.audit(ctx, "", "chat_session_title_generated", "agent", map[string]any{"session_id": sessionID, "title": title})
	return session, true, nil
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
	if s.hasActiveWorkspaceShellForSession(sessionID) {
		return domain.ChatSession{}, fmt.Errorf("conversation %q has an active Workspace terminal", sessionID)
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

func (s *Service) ListChatMessagesPage(ctx context.Context, sessionID string, limit int, beforeCreatedAt, beforeID string) (domain.ChatMessagePage, error) {
	return s.store.ListChatMessagesPage(ctx, strings.TrimSpace(sessionID), limit, strings.TrimSpace(beforeCreatedAt), strings.TrimSpace(beforeID))
}

func (s *Service) GetChatMessage(ctx context.Context, sessionID, messageID string) (domain.ChatMessage, error) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(messageID) == "" {
		return domain.ChatMessage{}, store.ErrNotFound
	}
	return s.store.GetChatMessage(ctx, strings.TrimSpace(sessionID), strings.TrimSpace(messageID))
}

func (s *Service) ListChatToolCalls(ctx context.Context, sessionID string) ([]domain.ChatToolCall, error) {
	return s.store.ListChatToolCalls(ctx, strings.TrimSpace(sessionID))
}

func (s *Service) CountRunningChatToolCalls(ctx context.Context, sessionID string) (int, error) {
	return s.store.CountRunningChatToolCalls(ctx, strings.TrimSpace(sessionID))
}

func (s *Service) GetChatAttachment(ctx context.Context, sessionID, attachmentID string) (domain.ChatAttachment, error) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(attachmentID) == "" {
		return domain.ChatAttachment{}, store.ErrNotFound
	}
	return s.store.GetChatAttachment(ctx, sessionID, attachmentID)
}

func (s *Service) DeleteChatSession(ctx context.Context, sessionID string, actors ...string) error {
	if calls, err := s.store.ListChatToolCalls(ctx, sessionID); err != nil {
		return err
	} else {
		for _, call := range calls {
			if call.Status == domain.ChatToolCallRunning {
				return fmt.Errorf("conversation %q has a running function tool; stop it before deleting the conversation", sessionID)
			}
		}
	}
	if s.hasActiveSSHShellForSession(sessionID) {
		return fmt.Errorf("conversation %q has an active terminal; close it before deleting the conversation", sessionID)
	}
	if s.hasActiveTaskForSession(sessionID) {
		return fmt.Errorf("conversation %q has an active background task; cancel it before deleting the conversation", sessionID)
	}
	if err := s.store.DeleteChatSession(ctx, sessionID); err != nil {
		return err
	}
	actor := ""
	if len(actors) > 0 {
		actor = actors[0]
	}
	s.audit(ctx, "", "chat_session_deleted", actor, map[string]any{"session_id": sessionID})
	return nil
}

func (s *Service) hasActiveTaskForSession(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	s.taskMu.RLock()
	defer s.taskMu.RUnlock()
	for _, state := range s.tasks {
		if state.sessionID == sessionID {
			return true
		}
	}
	return false
}

func (s *Service) GetAgentTasks(ctx context.Context, sessionID string) (domain.AgentTaskList, error) {
	if strings.TrimSpace(sessionID) == "" {
		sessionID = SessionIDFromContext(ctx)
	}
	if sessionID == "" {
		return domain.AgentTaskList{}, fmt.Errorf("agent tasks require a session context")
	}
	return s.store.ListAgentTasks(ctx, sessionID)
}

func currentAgentTask(tasks domain.AgentTaskList) string {
	for _, task := range tasks.Items {
		if task.Status == "in_progress" {
			return fmt.Sprintf("#%s %s", task.ID, task.Subject)
		}
	}
	for _, task := range tasks.Items {
		if task.Status == "pending" && !agentTaskBlocked(tasks, task) {
			return fmt.Sprintf("#%s %s", task.ID, task.Subject)
		}
	}
	return ""
}

func agentTaskBlocked(tasks domain.AgentTaskList, task domain.AgentTask) bool {
	for _, blockerID := range task.BlockedBy {
		resolved := false
		for _, candidate := range tasks.Items {
			if candidate.ID == blockerID {
				resolved = candidate.Status == "completed"
				break
			}
		}
		if !resolved {
			return true
		}
	}
	return false
}

func (s *Service) SaveModelProvider(ctx context.Context, input domain.ModelProviderInput, actor string) (domain.ModelProvider, error) {
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Kind = strings.TrimSpace(input.Kind)
	input.BaseURL = strings.TrimSpace(input.BaseURL)
	input.Model = strings.TrimSpace(input.Model)
	input.ProxyID = strings.TrimSpace(input.ProxyID)
	contextWindow := 0
	if input.ContextWindow != nil {
		contextWindow = *input.ContextWindow
		if contextWindow != 0 && (contextWindow < domain.MinModelContextWindow || contextWindow > domain.MaxModelContextWindow) {
			return domain.ModelProvider{}, fmt.Errorf("context_window must be between %d and %d", domain.MinModelContextWindow, domain.MaxModelContextWindow)
		}
	}
	reasoningEffort := ""
	if input.ReasoningEffort != nil {
		var err error
		reasoningEffort, err = normalizeReasoningEffort(*input.ReasoningEffort)
		if err != nil {
			return domain.ModelProvider{}, err
		}
	}
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
	if input.ProxyID != "" {
		if _, err := s.store.GetProxy(ctx, input.ProxyID); err != nil {
			return domain.ModelProvider{}, fmt.Errorf("load proxy %q: %w", input.ProxyID, err)
		}
	}

	var existing domain.ModelProvider
	if input.ID != "" {
		existing, err = s.store.GetModelProvider(ctx, input.ID)
		if err != nil {
			return domain.ModelProvider{}, err
		}
	}
	provider := domain.ModelProvider{
		ID: input.ID, Name: input.Name, Kind: input.Kind, BaseURL: input.BaseURL, Model: input.Model,
		ContextWindow: contextWindow,
		ProxyID:       input.ProxyID, UserAgent: userAgent, ReasoningEffort: reasoningEffort,
	}
	if existing.ID != "" {
		provider.CreatedAt = existing.CreatedAt
		provider.Active = existing.Active
		provider.APIKeyCipher = existing.APIKeyCipher
		if input.ContextWindow == nil {
			provider.ContextWindow = existing.ContextWindow
		}
		if input.UserAgent == nil {
			provider.UserAgent = existing.UserAgent
		}
		if input.ReasoningEffort == nil {
			provider.ReasoningEffort = existing.ReasoningEffort
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
	saved, err := s.store.UpsertModelProvider(ctx, provider)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
			return domain.ModelProvider{}, fmt.Errorf("provider name already exists")
		}
		return domain.ModelProvider{}, err
	}
	s.audit(ctx, "", "model_provider_saved", actor, map[string]any{
		"provider_id": saved.ID, "name": saved.Name, "kind": saved.Kind, "model": saved.Model,
		"proxy_id": saved.ProxyID, "reasoning_effort": saved.ReasoningEffort,
		"context_window": saved.ContextWindow,
	})
	return saved, nil
}

func (s *Service) ListModelProviders(ctx context.Context) ([]domain.ModelProvider, error) {
	providers, err := s.store.ListModelProviders(ctx)
	if err != nil {
		return nil, err
	}
	for index := range providers {
		if providers[index].ContextWindow != 0 {
			continue
		}
		metadata, exists := s.cachedModelMetadata(providers[index].Kind, providers[index].Model)
		if exists {
			providers[index].ResolvedContextWindow = metadata.ContextWindow
		}
	}
	return providers, nil
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
	if input.ApprovalMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*input.ApprovalMode))
		switch mode {
		case domain.ApprovalModeManual, domain.ApprovalModeAuto, domain.ApprovalModeFullAccess:
			current.ApprovalMode = mode
		default:
			return domain.SystemSettings{}, fmt.Errorf("approval_mode must be manual, auto, or full_access")
		}
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
	if input.AutomaticApprovalModelProviderID != nil {
		providerID := strings.TrimSpace(*input.AutomaticApprovalModelProviderID)
		if providerID != "" {
			if _, err := s.store.GetModelProvider(ctx, providerID); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return domain.SystemSettings{}, fmt.Errorf("Auto approval model provider %q not found", providerID)
				}
				return domain.SystemSettings{}, err
			}
		}
		current.AutomaticApprovalModelProviderID = providerID
	}
	if input.SubagentTimeoutSeconds != nil {
		if *input.SubagentTimeoutSeconds < domain.MinSubagentTimeoutSeconds || *input.SubagentTimeoutSeconds > domain.MaxSubagentTimeoutSeconds {
			return domain.SystemSettings{}, fmt.Errorf("subagent_timeout_seconds must be between %d and %d", domain.MinSubagentTimeoutSeconds, domain.MaxSubagentTimeoutSeconds)
		}
		current.SubagentTimeoutSeconds = *input.SubagentTimeoutSeconds
	}
	if input.ContextCompressionEnabled != nil {
		current.ContextCompressionEnabled = *input.ContextCompressionEnabled
	}
	if input.ContextCompressionPercent != nil {
		if *input.ContextCompressionPercent < domain.MinContextCompressionPercent || *input.ContextCompressionPercent > domain.MaxContextCompressionPercent {
			return domain.SystemSettings{}, fmt.Errorf("context_compression_threshold_percent must be between %d and %d", domain.MinContextCompressionPercent, domain.MaxContextCompressionPercent)
		}
		current.ContextCompressionPercent = *input.ContextCompressionPercent
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
			if mode != current.WorkspaceShellMode && s.hasAnyActiveWorkspaceShell() {
				return domain.SystemSettings{}, fmt.Errorf("close active Workspace terminals before changing workspace_shell_mode")
			}
			current.WorkspaceShellMode = mode
		default:
			return domain.SystemSettings{}, fmt.Errorf("workspace_shell_mode must be sandbox, host, or disabled")
		}
	}
	rotatedMCPHTTPToken := false
	var mcpHTTPToken string
	if input.MCPHTTPEnabled != nil {
		wasEnabled := current.MCPHTTPEnabled
		current.MCPHTTPEnabled = *input.MCPHTTPEnabled
		if current.MCPHTTPEnabled && (!wasEnabled || current.MCPHTTPTokenHash == "") {
			input.RotateMCPHTTPToken = true
		}
	}
	if input.RotateMCPHTTPToken {
		if !current.MCPHTTPEnabled {
			return domain.SystemSettings{}, fmt.Errorf("MCP HTTP server must be enabled before rotating its token")
		}
		mcpHTTPToken, err = generateMCPHTTPToken()
		if err != nil {
			return domain.SystemSettings{}, err
		}
		current.MCPHTTPTokenHash = hashMCPHTTPToken(mcpHTTPToken)
		rotatedMCPHTTPToken = true
	}
	saved, err := s.store.SaveSystemSettings(ctx, current)
	if err != nil {
		return domain.SystemSettings{}, err
	}
	s.audit(ctx, "", "system_settings_updated", actor, map[string]any{
		"agent_max_iterations": saved.AgentMaxIterations, "approval_mode": saved.ApprovalMode,
		"approval_explanations_enabled": saved.ApprovalExplanationsEnabled,
		"system_prompt_changed":         systemPromptChanged, "system_prompt_bytes": len(saved.SystemPrompt),
		"subagent_model_provider_id": saved.SubagentModelProviderID, "subagent_timeout_seconds": saved.SubagentTimeoutSeconds,
		"automatic_approval_model_provider_id":  saved.AutomaticApprovalModelProviderID,
		"context_compression_enabled":           saved.ContextCompressionEnabled,
		"context_compression_threshold_percent": saved.ContextCompressionPercent,
		"chat_image_allowed_types":              saved.ChatImageAllowedTypes,
		"workspace_shell_mode":                  saved.WorkspaceShellMode,
		"mcp_http_enabled":                      saved.MCPHTTPEnabled,
		"mcp_http_token_rotated":                rotatedMCPHTTPToken,
	})
	saved.MCPHTTPToken = mcpHTTPToken
	return s.decorateWorkspaceShellSettings(saved), nil
}

func generateMCPHTTPToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate MCP HTTP token: %w", err)
	}
	return "opsnerva_mcp_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func hashMCPHTTPToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Service) MCPHTTPAccess(ctx context.Context, token string) (enabled bool, authorized bool, err error) {
	settings, err := s.store.GetSystemSettings(ctx)
	if err != nil {
		return false, false, err
	}
	if !settings.MCPHTTPEnabled {
		return false, false, nil
	}
	token = strings.TrimSpace(token)
	if token == "" || settings.MCPHTTPTokenHash == "" {
		return true, false, nil
	}
	expected, decodeErr := hex.DecodeString(settings.MCPHTTPTokenHash)
	if decodeErr != nil || len(expected) != sha256.Size {
		return true, false, nil
	}
	actual := sha256.Sum256([]byte(token))
	return true, subtle.ConstantTimeCompare(expected, actual[:]) == 1, nil
}

func (s *Service) SetApprovalReviewer(reviewer ApprovalReviewer) {
	s.reviewerMu.Lock()
	s.reviewer = reviewer
	s.reviewerMu.Unlock()
}

func (s *Service) approvalReviewer() ApprovalReviewer {
	s.reviewerMu.RLock()
	defer s.reviewerMu.RUnlock()
	return s.reviewer
}

func (s *Service) SetAutomaticApprovalReviewer(reviewer AutomaticApprovalReviewer) {
	s.reviewerMu.Lock()
	s.automaticReviewer = reviewer
	s.reviewerMu.Unlock()
}

func (s *Service) automaticApprovalReviewer() AutomaticApprovalReviewer {
	s.reviewerMu.RLock()
	defer s.reviewerMu.RUnlock()
	return s.automaticReviewer
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
	proxy, err := s.resolveProxy(ctx, provider.ProxyID)
	if err != nil {
		return config.Model{}, domain.ModelProvider{}, err
	}
	return config.Model{
		APIKey: string(key), Kind: provider.Kind, BaseURL: provider.BaseURL, Name: provider.Model, ContextWindow: provider.ContextWindow, ReasoningEffort: provider.ReasoningEffort, UserAgent: provider.UserAgent,
		ProxyURL: proxy.URL, ProxyUsername: proxy.Username, ProxyPassword: proxy.Password,
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
		return false, fmt.Errorf("%w: %q is selected for the approval Agent; choose another provider in system settings before deleting it", ErrModelProviderInUse, provider.Name)
	}
	if settings.AutomaticApprovalModelProviderID == provider.ID {
		return false, fmt.Errorf("%w: %q is selected for the Auto approval Agent; choose another provider in system settings before deleting it", ErrModelProviderInUse, provider.Name)
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
	agentEnabled := host.AgentEnabled
	return s.SaveHost(ctx, domain.HostInput{
		ID: host.ID, Name: host.Name, Address: host.Address, Port: host.Port, User: host.User,
		AgentEnabled: &agentEnabled,
		AuthType:     host.AuthType, KnownHostsFile: host.KnownHostsFile, ProxyJumpHostID: host.ProxyJumpHostID,
		ProxyID: host.ProxyID, SudoMode: host.SudoMode,
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
	input.ProxyID = strings.TrimSpace(input.ProxyID)
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
	if input.ProxyID != "" {
		proxy, err := s.store.GetProxy(ctx, input.ProxyID)
		if err != nil {
			return domain.Host{}, fmt.Errorf("load proxy %q: %w", input.ProxyID, err)
		}
		if _, err := sshx.NormalizeProxyURL(proxy.URL); err != nil {
			return domain.Host{}, fmt.Errorf("proxy %q is not compatible with SSH: %w", proxy.Name, err)
		}
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
	if containsCredentialControl(input.Password) || containsCredentialControl(input.SudoPassword) {
		return domain.Host{}, fmt.Errorf("credentials cannot contain NUL, carriage return, or newline characters")
	}
	if len(input.Password) > 1024 || len(input.SudoPassword) > 1024 {
		return domain.Host{}, fmt.Errorf("password is too long")
	}
	if input.AuthType != "key" {
		input.PrivateKey = ""
	}
	agentEnabled := true
	if hasExisting {
		agentEnabled = existing.AgentEnabled
	}
	if input.AgentEnabled != nil {
		agentEnabled = *input.AgentEnabled
	}

	host := domain.Host{
		ID: input.ID, Name: input.Name, Address: input.Address, Port: input.Port, User: input.User,
		AgentEnabled: agentEnabled,
		AuthType:     input.AuthType, KnownHostsFile: input.KnownHostsFile, ProxyJumpHostID: input.ProxyJumpHostID,
		ProxyID: input.ProxyID, SudoMode: input.SudoMode,
	}
	if hasExisting {
		host.CreatedAt = existing.CreatedAt
		host.PasswordCipher = existing.PasswordCipher
		host.SudoCipher = existing.SudoCipher
		host.PrivateKeyCipher = existing.PrivateKeyCipher
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
		"host_id": created.ID, "name": created.Name, "agent_enabled": created.AgentEnabled, "auth_type": created.AuthType, "has_private_key": created.HasPrivateKey, "sudo_mode": created.SudoMode,
	})
	return created, nil
}

func (s *Service) GetHost(ctx context.Context, id string) (domain.Host, error) {
	host, err := s.store.GetHost(ctx, id)
	if err != nil {
		return domain.Host{}, err
	}
	return s.withStoredHostKey(host), nil
}

func (s *Service) ListHosts(ctx context.Context) ([]domain.Host, error) {
	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	for index := range hosts {
		hosts[index] = s.withStoredHostKey(hosts[index])
	}
	return hosts, nil
}

func (s *Service) withStoredHostKey(host domain.Host) domain.Host {
	key, ok := s.transport.StoredHostKey(host)
	if ok {
		host.HostKey = &domain.HostKey{
			Fingerprint: key.Fingerprint,
			Algorithm:   key.Algorithm,
			Trusted:     key.Trusted,
		}
	}
	return host
}

func (s *Service) ListHostCapabilities(ctx context.Context) ([]domain.HostCapability, error) {
	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]domain.HostCapability, 0, len(hosts))
	for _, host := range hosts {
		if !host.AgentEnabled {
			continue
		}
		connection, _, resolveErr := s.resolveSSHConnection(ctx, host)
		if resolveErr == nil && requireAgentSSHAccess("eino-agent", connection) != nil {
			continue
		}
		result = append(result, domain.HostCapability{ID: host.ID, Name: host.Name, AuthType: host.AuthType, SudoMode: host.SudoMode})
	}
	return result, nil
}

func (s *Service) DeleteHost(ctx context.Context, id, actor string) error {
	if s.hasSSHTunnelForHost(id) {
		return fmt.Errorf("%w: stop the tunnel before deleting host %q", ErrHostHasActiveTunnel, id)
	}
	if s.hasActiveSSHShellForHost(id) {
		return fmt.Errorf("host %q has an active SSH shell; close it before deleting the host", id)
	}
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

func (s *Service) ProbeHost(ctx context.Context, id, actor string) (sshx.HostInfo, error) {
	host, err := s.store.GetHost(ctx, id)
	if err != nil {
		return sshx.HostInfo{}, err
	}
	connection, _, err := s.resolveSSHConnection(ctx, host)
	if err != nil {
		return sshx.HostInfo{}, err
	}
	if err := requireAgentSSHAccess(actor, connection); err != nil {
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
	if isWorkspaceMode(req.Mode) {
		if req.SSHConnectionDigest != "" || req.SourceConnectionDigest != "" {
			return domain.ExecResult{}, fmt.Errorf("SSH connection binding is invalid for local Workspace operations")
		}
	} else if req.Mode == domain.ExecSSHFileTransfer {
		_, err = s.bindSSHFileTransfer(ctx, host, &req, actor)
		if err != nil {
			return domain.ExecResult{}, err
		}
	} else {
		if req.SourceConnectionDigest != "" {
			return domain.ExecResult{}, fmt.Errorf("source SSH connection binding is only valid for host-to-host transfers")
		}
		connection, digest, connectionErr := s.resolveSSHConnection(ctx, host)
		if connectionErr != nil {
			return domain.ExecResult{}, connectionErr
		}
		if err := requireAgentSSHAccess(actor, connection); err != nil {
			return domain.ExecResult{}, err
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
	sessionID := SessionIDFromContext(ctx)
	if (req.Mode == domain.ExecSSHShellStart || req.Mode == domain.ExecWorkspaceShellStart) && sessionID == "" {
		return domain.ExecResult{}, fmt.Errorf("interactive shells require an Agent conversation")
	}
	settings, settingsErr := s.store.GetSystemSettings(ctx)
	if settingsErr != nil {
		return domain.ExecResult{}, settingsErr
	}
	llmRequest := actor == "eino-agent" || actor == "mcp-client"
	approvalRequired := llmRequest && settings.ApprovalMode != domain.ApprovalModeFullAccess
	requestCipher, err := s.encryptor.Encrypt([]byte(requestJSON))
	if err != nil {
		return domain.ExecResult{}, err
	}
	requestRedacted := s.redactor.Redact(requestJSON)
	now := time.Now().UTC()
	var commandExplanation *domain.CommandReview
	var explanationInput *domain.CommandReviewInput
	var reviewer ApprovalReviewer
	autoRejected := false
	if approvalRequired {
		if llmRequest && settings.ApprovalMode == domain.ApprovalModeAuto {
			input := s.automaticApprovalInput(ctx, req, host, digest, sessionID)
			review := s.reviewForAutomaticApproval(ctx, s.automaticApprovalReviewer(), input, settings.SubagentTimeoutSeconds)
			commandExplanation = &review
			switch {
			case review.Status == "completed" && review.Decision == domain.ApprovalAgentAllow:
				approvalRequired = false
			case review.Status == "completed" && review.Decision == domain.ApprovalAgentReject:
				autoRejected = true
			case review.Status == "completed" && review.Decision == domain.ApprovalAgentManual:
				approvalRequired = true
			}
		} else {
			reviewer = s.approvalReviewer()
			if settings.ApprovalExplanationsEnabled && reviewer != nil {
				input := s.commandReviewInput(ctx, req, host, digest, sessionID)
				explanationInput = &input
				commandExplanation = &domain.CommandReview{Status: "pending"}
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
		Status: "created", AIReviewJSON: reviewJSON, AIReview: commandExplanation, StartedAt: now,
	}
	if owner, ok := executionOwnerFromContext(ctx); ok {
		run.ToolName = owner.ToolName
		run.ToolArgumentsJSON = s.redactor.Redact(owner.Arguments)
	}
	logger := observability.FromContext(ctx).With(
		"session_id", sessionID, "host_id", host.ID,
		"mode", req.Mode, "program", req.Program, "elevated", req.Elevated,
		"actor", actor, "run_id", run.ID,
	)
	logger.DebugContext(ctx, "approval route selected", "approval_mode", settings.ApprovalMode, "approval_required", approvalRequired, "request_digest", digest)
	if autoRejected {
		run.Status = "rejected"
		run.Error = commandExplanation.Reason
		run.CompletedAt = time.Now().UTC()
		if err := s.store.CreateRun(ctx, run); err != nil {
			return domain.ExecResult{}, err
		}
		s.audit(ctx, run.ID, "auto_approval_agent_rejected", "auto-approval-agent", map[string]any{
			"reason": commandExplanation.Reason, "model": commandExplanation.Model,
		})
		logger.With("component", "approval").InfoContext(ctx, "Auto approval Agent rejected execution", "model", commandExplanation.Model)
		return execResultFromRun(run, "", ""), nil
	}
	if approvalRequired {
		run.Status = "approval_required"
		if err := s.store.CreateRun(ctx, run); err != nil {
			return domain.ExecResult{}, err
		}
		if owner, ok := executionOwnerFromContext(ctx); ok {
			s.bindExecutionOwner(ctx, run.ID, sessionID, owner)
		}
		approval := domain.Approval{
			ID: ids.New("approval"), RunID: run.ID, HostID: host.ID, RequestJSON: requestRedacted, RequestCipher: requestCipher,
			RequestDigest: digest, Status: "pending",
			CreatedAt: now,
		}
		if err := s.store.CreateApproval(ctx, approval); err != nil {
			s.clearExecutionOwner(run.ID)
			return domain.ExecResult{}, err
		}
		s.audit(ctx, run.ID, "approval_requested", actor, map[string]any{"approval_id": approval.ID, "mode": settings.ApprovalMode})
		if commandExplanation != nil && commandExplanation.Status == "completed" && commandExplanation.Decision == domain.ApprovalAgentManual {
			s.audit(ctx, run.ID, "auto_approval_agent_requested_manual_review", "auto-approval-agent", map[string]any{
				"approval_id": approval.ID, "reason": commandExplanation.Reason, "model": commandExplanation.Model,
			})
		}
		logger.With("component", "approval").InfoContext(ctx, "execution awaiting approval", "approval_id", approval.ID, "approval_mode", settings.ApprovalMode)
		if commandExplanation != nil && commandExplanation.Status == "pending" && explanationInput != nil && reviewer != nil {
			s.startPendingApprovalExplanation(ctx, approval, *explanationInput, reviewer, settings.SubagentTimeoutSeconds)
		}
		return domain.ExecResult{RunID: run.ID, Status: run.Status, ApprovalID: approval.ID}, nil
	}
	run.Status = "running"
	if err := s.store.CreateRun(ctx, run); err != nil {
		return domain.ExecResult{}, err
	}
	if owner, ok := executionOwnerFromContext(ctx); ok {
		s.bindExecutionOwner(ctx, run.ID, sessionID, owner)
	}
	if commandExplanation != nil && commandExplanation.Status == "completed" && commandExplanation.Decision == domain.ApprovalAgentAllow {
		s.audit(ctx, run.ID, "auto_approval_agent_granted", "auto-approval-agent", map[string]any{
			"reason": commandExplanation.Reason, "model": commandExplanation.Model,
		})
	}
	return s.execute(ctx, host, req, run, actor, stream)
}

func (s *Service) commandReviewInput(ctx context.Context, req domain.ExecRequest, host domain.Host, digest, sessionID string) domain.CommandReviewInput {
	currentTask := ""
	if sessionID != "" {
		if tasks, err := s.store.ListAgentTasks(ctx, sessionID); err == nil {
			currentTask = currentAgentTask(tasks)
		}
	}
	return domain.CommandReviewInput{
		Request:       req,
		Host:          domain.HostCapability{ID: host.ID, Name: host.Name, AuthType: host.AuthType, SudoMode: host.SudoMode},
		CurrentTask:   currentTask,
		RequestDigest: digest,
	}
}

func (s *Service) automaticApprovalInput(ctx context.Context, req domain.ExecRequest, host domain.Host, digest, sessionID string) domain.AutomaticApprovalInput {
	input := domain.AutomaticApprovalInput{
		Request: req, Host: domain.HostCapability{ID: host.ID, Name: host.Name, AuthType: host.AuthType, SudoMode: host.SudoMode},
		UserRequest: s.redactor.Redact(approvalUserRequestFromContext(ctx)), RequestDigest: digest,
	}
	if sessionID == "" {
		return input
	}
	if tasks, err := s.store.ListAgentTasks(ctx, sessionID); err == nil {
		input.CurrentTask = s.redactor.Redact(currentAgentTask(tasks))
	}
	return input
}

func (s *Service) reviewForAutomaticApproval(ctx context.Context, reviewer AutomaticApprovalReviewer, input domain.AutomaticApprovalInput, timeoutSeconds int) domain.CommandReview {
	if strings.TrimSpace(input.UserRequest) == "" {
		return markAutomaticApprovalReview(s.normalizeCommandReview(domain.CommandReview{}, fmt.Errorf("current user request is unavailable for Auto approval"), timeoutSeconds))
	}
	if reviewer == nil {
		return markAutomaticApprovalReview(s.normalizeCommandReview(domain.CommandReview{}, fmt.Errorf("Auto approval Agent is unavailable for the active model"), timeoutSeconds))
	}
	timeoutSeconds = effectiveSubagentTimeoutSeconds(timeoutSeconds)
	reviewCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	select {
	case s.automaticApprovalSem <- struct{}{}:
		defer func() { <-s.automaticApprovalSem }()
	case <-reviewCtx.Done():
		return markAutomaticApprovalReview(s.normalizeCommandReview(domain.CommandReview{}, reviewCtx.Err(), timeoutSeconds))
	}
	review, err := reviewer.Review(reviewCtx, input)
	review = s.normalizeCommandReview(review, err, timeoutSeconds)
	if review.Status == "completed" && review.Decision != domain.ApprovalAgentAllow && review.Decision != domain.ApprovalAgentReject && review.Decision != domain.ApprovalAgentManual {
		review.Status = "degraded"
		review.Errors = append(review.Errors, "Auto approval Agent returned an invalid decision")
	}
	if review.Status == "completed" && strings.TrimSpace(review.Reason) == "" {
		review.Status = "degraded"
		review.Errors = append(review.Errors, "Auto approval Agent returned no reason")
	}
	if review.Status == "completed" && (review.Explanation == nil || strings.TrimSpace(review.Explanation.Summary) == "" || strings.TrimSpace(review.Explanation.Mechanism) == "") {
		review.Status = "degraded"
		review.Errors = append(review.Errors, "Auto approval Agent returned no operation explanation")
	}
	return markAutomaticApprovalReview(review)
}

func markAutomaticApprovalReview(review domain.CommandReview) domain.CommandReview {
	review.Kind = domain.CommandReviewKindAutomaticApproval
	return review
}

// startPendingApprovalExplanation keeps model latency outside the human
// approval response path. Explanation work is bounded globally and canceled as
// soon as its approval is no longer pending.
func (s *Service) startPendingApprovalExplanation(parent context.Context, approval domain.Approval, input domain.CommandReviewInput, reviewer ApprovalReviewer, timeoutSeconds int) {
	baseCtx := context.WithoutCancel(parent)
	timeoutSeconds = effectiveSubagentTimeoutSeconds(timeoutSeconds)
	logger := observability.FromContext(baseCtx).With(
		"component", "approval", "approval_id", approval.ID, "run_id", approval.RunID,
	)
	select {
	case s.explanationSlots <- struct{}{}:
	default:
		review := domain.CommandReview{
			Status: "unavailable",
			Errors: []string{"approval Agent review skipped because the local queue is full"}, ReviewedAt: time.Now().UTC(),
		}
		persistCtx, cancelPersist := context.WithTimeout(baseCtx, 3*time.Second)
		err := s.persistPendingApprovalExplanation(persistCtx, approval, review, 0)
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
				logger.ErrorContext(baseCtx, "approval Agent panicked", "panic", fmt.Sprint(recovered))
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
			review := s.normalizeCommandReview(domain.CommandReview{}, explanationCtx.Err(), timeoutSeconds)
			persistCtx, cancelPersist := context.WithTimeout(baseCtx, 3*time.Second)
			err := s.persistPendingApprovalExplanation(persistCtx, approval, review, time.Since(queuedAt))
			cancelPersist()
			if err != nil {
				logger.ErrorContext(baseCtx, "persist queued approval explanation timeout failed", "error", err)
			}
			return
		}

		started := time.Now()
		logger.InfoContext(baseCtx, "approval explanation started", "queue_ms", started.Sub(queuedAt).Milliseconds())
		review, reviewErr := reviewer.Review(explanationCtx, input)
		if errors.Is(explanationCtx.Err(), context.Canceled) {
			logger.InfoContext(baseCtx, "approval explanation canceled", "duration_ms", time.Since(started).Milliseconds())
			return
		}
		review = s.normalizeCommandReview(review, reviewErr, timeoutSeconds)
		persistCtx, cancelPersist := context.WithTimeout(baseCtx, 3*time.Second)
		err := s.persistPendingApprovalExplanation(persistCtx, approval, review, time.Since(started))
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

func (s *Service) persistPendingApprovalExplanation(ctx context.Context, approval domain.Approval, review domain.CommandReview, duration time.Duration) error {
	reviewJSON, err := json.Marshal(review)
	if err != nil {
		return fmt.Errorf("encode approval explanation: %w", err)
	}
	if err := s.store.UpdatePendingApprovalExplanation(ctx, approval.ID, approval.RunID, string(reviewJSON)); err != nil {
		return err
	}
	s.audit(ctx, approval.RunID, "approval_agent_reviewed", "approval-agent", map[string]any{
		"approval_id": approval.ID, "status": review.Status,
		"model": review.Model, "duration_ms": duration.Milliseconds(),
	})
	notifyApproval(ctx, domain.ExecResult{
		RunID: approval.RunID, Status: "approval_required",
		ApprovalID: approval.ID,
	})
	return nil
}

func (s *Service) normalizeCommandReview(review domain.CommandReview, reviewErr error, timeoutSeconds int) domain.CommandReview {
	if reviewErr != nil {
		model := review.Model
		message := reviewErr.Error()
		if errors.Is(reviewErr, context.DeadlineExceeded) || strings.Contains(strings.ToLower(message), "context deadline exceeded") {
			message = fmt.Sprintf("approval Agent did not respond within %d seconds", effectiveSubagentTimeoutSeconds(timeoutSeconds))
		}
		review = domain.CommandReview{
			Status: "unavailable", Model: model,
			Errors: []string{message}, ReviewedAt: time.Now().UTC(),
		}
	}
	if review.Status != "completed" && review.Status != "degraded" && review.Status != "unavailable" {
		review.Status = "degraded"
	}
	if review.ReviewedAt.IsZero() {
		review.ReviewedAt = time.Now().UTC()
	}
	review.Decision = strings.ToLower(strings.TrimSpace(review.Decision))
	review.Reason = s.redactor.Redact(strings.TrimSpace(review.Reason))
	if len(review.Reason) > 1000 {
		review.Reason = review.Reason[:1000]
	}
	if review.Explanation != nil {
		review.Explanation.Summary = s.redactor.Redact(review.Explanation.Summary)
		review.Explanation.Mechanism = s.redactor.Redact(review.Explanation.Mechanism)
		for index := range review.Explanation.Risks {
			review.Explanation.Risks[index] = s.redactor.Redact(review.Explanation.Risks[index])
		}
	}
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
	approved, err := s.approveForExecution(ctx, approvalID, reason, actor)
	if err != nil {
		return domain.ExecResult{}, err
	}
	return s.executeApproved(ctx, approved)
}

func (s *Service) ApproveAsync(ctx context.Context, approvalID, reason, actor string) (domain.ExecResult, error) {
	approved, err := s.approveForExecution(ctx, approvalID, reason, actor)
	if err != nil {
		return domain.ExecResult{}, err
	}
	if err := s.startApprovedExecution(ctx, approved); err != nil {
		s.finishApprovedExecutionError(approved, err)
		return domain.ExecResult{}, err
	}
	return execResultFromRun(approved.run, approved.approval.ID, ""), nil
}

type approvedExecution struct {
	approval domain.Approval
	request  domain.ExecRequest
	run      domain.Run
	host     domain.Host
	actor    string
}

func (s *Service) approveForExecution(ctx context.Context, approvalID, reason, actor string) (approvedExecution, error) {
	logger := observability.FromContext(ctx).With("component", "approval", "approval_id", approvalID, "actor", actor)
	approval, err := s.store.GetApproval(ctx, approvalID)
	if err != nil {
		return approvedExecution{}, err
	}
	if approval.Status != "pending" {
		logger.WarnContext(ctx, "approval decision ignored", "status", approval.Status)
		return approvedExecution{}, fmt.Errorf("approval is %s", approval.Status)
	}
	requestData, err := s.encryptor.Decrypt(approval.RequestCipher)
	if err != nil {
		return approvedExecution{}, err
	}
	if len(requestData) == 0 {
		requestData = []byte(approval.RequestJSON)
	}
	var req domain.ExecRequest
	if err := json.Unmarshal(requestData, &req); err != nil {
		return approvedExecution{}, err
	}
	_, digest, err := canonicalRequest(req)
	if err != nil || digest != approval.RequestDigest {
		return approvedExecution{}, fmt.Errorf("approved request digest no longer matches")
	}
	run, err := s.store.GetRun(ctx, approval.RunID)
	if err != nil {
		return approvedExecution{}, err
	}
	host, err := s.store.GetHost(ctx, approval.HostID)
	if err != nil {
		return approvedExecution{}, err
	}
	if err := s.store.ApprovePendingAndStartRun(ctx, approval.ID, run.ID, reason); err != nil {
		return approvedExecution{}, err
	}
	s.cancelApprovalExplanation(ctx, approval.ID, approval.RunID)
	run.Status = "running"
	s.audit(ctx, run.ID, "approval_granted", actor, map[string]any{"approval_id": approval.ID, "reason": reason, "session_id": approval.SessionID})
	logger.InfoContext(ctx, "approval granted", "run_id", run.ID, "session_id", approval.SessionID)
	return approvedExecution{approval: approval, request: req, run: run, host: host, actor: actor}, nil
}

func (s *Service) startApprovedExecution(parent context.Context, approved approvedExecution) error {
	executionCtx, cancel := context.WithCancel(context.WithoutCancel(parent))
	s.executionMu.Lock()
	if s.executionClosed {
		s.executionMu.Unlock()
		cancel()
		return fmt.Errorf("service is shutting down")
	}
	if _, cancelled := s.cancelledExecutions[approved.run.ID]; cancelled {
		delete(s.cancelledExecutions, approved.run.ID)
		s.executionMu.Unlock()
		cancel()
		return context.Canceled
	}
	s.executionCancels[approved.run.ID] = cancel
	s.executionWG.Add(1)
	s.executionMu.Unlock()

	stopServiceCancellation := context.AfterFunc(s.executionCtx, cancel)
	go func() {
		defer s.executionWG.Done()
		defer stopServiceCancellation()
		defer cancel()
		defer func() {
			s.executionMu.Lock()
			delete(s.executionCancels, approved.run.ID)
			delete(s.cancelledExecutions, approved.run.ID)
			s.executionMu.Unlock()
		}()
		defer func() {
			if recovered := recover(); recovered != nil {
				err := fmt.Errorf("approved execution stopped unexpectedly")
				observability.FromContext(executionCtx).ErrorContext(executionCtx, "approved execution panicked", "run_id", approved.run.ID, "panic", s.redactor.Redact(fmt.Sprint(recovered)))
				s.finishApprovedExecutionError(approved, err)
			}
		}()
		_, _ = s.executeApproved(executionCtx, approved)
	}()
	return nil
}

func (s *Service) cancelApprovedExecution(runID string) bool {
	if runID == "" {
		return false
	}
	s.executionMu.Lock()
	cancel := s.executionCancels[runID]
	if cancel == nil {
		s.cancelledExecutions[runID] = struct{}{}
	}
	s.executionMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

func (s *Service) executeApproved(ctx context.Context, approved approvedExecution) (domain.ExecResult, error) {
	result, err := s.execute(ctx, approved.host, approved.request, approved.run, approved.actor, nil)
	if err == nil {
		return result, nil
	}
	s.finishApprovedExecutionError(approved, err)
	if result.RunID != "" {
		return result, err
	}
	run, loadErr := s.store.GetRun(context.WithoutCancel(ctx), approved.run.ID)
	if loadErr == nil {
		result = execResultFromRun(run, approved.approval.ID, "")
	}
	return result, err
}

func (s *Service) finishApprovedExecutionError(approved approvedExecution, cause error) {
	defer s.clearExecutionOwner(approved.run.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run, err := s.store.GetRun(ctx, approved.run.ID)
	if err != nil || (run.Status != "created" && run.Status != "approval_required" && run.Status != "running") {
		return
	}
	run.Status = "failed"
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		run.Status = "interrupted"
	}
	run.Error = s.redactor.Redact(cause.Error())
	run.CompletedAt = time.Now().UTC()
	if err := s.store.UpdateRun(ctx, run); err != nil {
		observability.FromContext(ctx).ErrorContext(ctx, "persist approved execution failure failed", "run_id", run.ID, "error", err)
		return
	}
	s.publishExecutionEvent(ExecutionEvent{SessionID: run.SessionID, RunID: run.ID, Status: run.Status})
	s.audit(ctx, run.ID, "command_completed", approved.actor, map[string]any{"status": run.Status, "error": run.Error})
	observability.FromContext(ctx).ErrorContext(ctx, "approved execution stopped before completion", "run_id", run.ID, "status", run.Status, "error", run.Error)
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
	s.publishExecutionEvent(ExecutionEvent{SessionID: run.SessionID, RunID: run.ID, Status: run.Status})
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

		switch approval.Status {
		case "rejected":
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
	result := domain.ExecResult{
		RunID: run.ID, Status: run.Status, ApprovalID: approvalID,
		AutoApproved:        autoApprovedRun(run),
		OperatorInstruction: operatorInstruction, ExitCode: run.ExitCode,
		Stdout: run.StdoutRedacted, Stderr: stderr,
		Duration: duration, CompletedAt: run.CompletedAt,
	}
	var request domain.ExecRequest
	if json.Unmarshal([]byte(run.RequestJSON), &request) == nil && request.Mode == domain.ExecSSHTunnelStart {
		var tunnel domain.SSHTunnel
		if json.Unmarshal([]byte(run.StdoutRedacted), &tunnel) == nil && tunnel.ID != "" {
			result.Tunnel = &tunnel
		}
	}
	if request.Mode == domain.ExecSSHShellStart || request.Mode == domain.ExecWorkspaceShellStart {
		var shell domain.SSHShell
		if json.Unmarshal([]byte(run.StdoutRedacted), &shell) == nil && shell.ID != "" {
			result.Shell = &shell
		}
		result.ShellUsage = sshShellUsage()
	}
	return result
}

func autoApprovedRun(run domain.Run) bool {
	return run.AIReview != nil && run.AIReview.Kind == domain.CommandReviewKindAutomaticApproval && run.AIReview.Status == "completed" && run.AIReview.Decision == domain.ApprovalAgentAllow
}

func (s *Service) execute(ctx context.Context, host domain.Host, req domain.ExecRequest, run domain.Run, actor string, stream func(string, []byte)) (domain.ExecResult, error) {
	defer s.clearExecutionOwner(run.ID)
	autoApproved := autoApprovedRun(run)
	logger := observability.FromContext(ctx).With(
		"component", "execution", "run_id", run.ID, "session_id", run.SessionID, "host_id", host.ID,
		"mode", req.Mode, "program", req.Program, "elevated", req.Elevated,
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
			return domain.ExecResult{RunID: run.ID, Status: run.Status, AutoApproved: autoApproved, Stderr: prepareErr.Error(), CompletedAt: run.CompletedAt}, prepareErr
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
			return domain.ExecResult{RunID: run.ID, Status: run.Status, AutoApproved: autoApproved, Stderr: prepareErr.Error(), Change: req.Change, CompletedAt: run.CompletedAt}, prepareErr
		}
		transportReq = prepared
	}
	hostIDs := []string{host.ID}
	if req.Mode == domain.ExecSSHFileTransfer {
		hostIDs = append(hostIDs, req.SourceHostID)
	}
	release := func() {}
	var err error
	release, err = s.acquire(ctx, hostIDs...)
	if err != nil {
		logger.WarnContext(ctx, "operation canceled before acquiring capacity", "error", err)
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
		return domain.ExecResult{RunID: run.ID, Status: run.Status, AutoApproved: autoApproved, CompletedAt: run.CompletedAt}, err
	}
	s.audit(ctx, run.ID, "command_started", actor, map[string]any{"digest": run.RequestDigest})
	s.publishExecutionEvent(ExecutionEvent{
		SessionID: run.SessionID,
		RunID:     run.ID,
		Status:    "running",
	})
	var raw sshx.RawResult
	var execErr error
	var tunnel *domain.SSHTunnel
	var shell *domain.SSHShell
	var outputSink *executionOutputSink
	if stream != nil || s.hasExecutionSubscribers(run.SessionID) {
		outputSink = s.newExecutionOutputSink(run, stream)
	}
	if req.Mode == domain.ExecSSHTunnelStart {
		started := time.Now()
		created, tunnelErr := s.openSSHTunnel(ctx, host, connection, req, actor)
		execErr = tunnelErr
		raw.Duration = time.Since(started)
		if tunnelErr == nil {
			tunnel = &created
			raw.ExitCode = 0
			raw.Stdout, execErr = marshalSSHTunnel(created)
		} else {
			raw.ExitCode = -1
		}
	} else if req.Mode == domain.ExecSSHShellStart {
		started := time.Now()
		created, shellErr := s.openSSHShell(ctx, host, connection, req, run, actor)
		execErr = shellErr
		raw.Duration = time.Since(started)
		if shellErr == nil {
			shell = &created
			raw.ExitCode = 0
			raw.Stdout, execErr = marshalSSHShell(created)
		} else {
			raw.ExitCode = -1
		}
	} else if req.Mode == domain.ExecWorkspaceShellStart {
		started := time.Now()
		created, shellErr := s.openWorkspaceShell(ctx, host, req, run, actor)
		execErr = shellErr
		raw.Duration = time.Since(started)
		if shellErr == nil {
			shell = &created
			raw.ExitCode = 0
			raw.Stdout, execErr = marshalSSHShell(created)
		} else {
			raw.ExitCode = -1
		}
	} else if req.Mode == domain.ExecSSHFileTransfer {
		raw, execErr = s.executeSSHFileTransfer(ctx, run, req)
	} else if req.Mode == domain.ExecWorkspaceDownload {
		raw, execErr = s.executeWorkspaceDownload(ctx, connection, req, actor)
	} else if isWorkspaceMode(req.Mode) {
		var workspaceStream func(string, []byte)
		if outputSink != nil {
			workspaceStream = outputSink.Write
		}
		raw, execErr = s.executeWorkspace(ctx, req, actor, workspaceStream)
	} else if streaming, ok := s.transport.(sshx.StreamingTransport); ok && outputSink != nil {
		raw, execErr = streaming.ExecStream(ctx, connection, transportReq, outputSink.Write)
	} else {
		raw, execErr = s.transport.Exec(ctx, connection, transportReq)
	}
	if outputSink != nil {
		outputSink.Flush()
	}
	run.ExitCode = raw.ExitCode
	run.StdoutRedacted = s.redactor.Redact(string(raw.Stdout))
	run.StderrRedacted = s.redactor.Redact(string(raw.Stderr))
	run.StdoutCipher, _ = s.encryptor.Encrypt(raw.Stdout)
	run.StderrCipher, _ = s.encryptor.Encrypt(raw.Stderr)
	run.CompletedAt = time.Now().UTC()
	if execErr != nil {
		run.Status = "failed"
		if errors.Is(execErr, context.Canceled) || errors.Is(execErr, context.DeadlineExceeded) ||
			errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			run.Status = "interrupted"
		}
		run.Error = execErr.Error()
	} else if raw.ExitCode != 0 {
		run.Error = "remote command exited with code " + strconv.Itoa(raw.ExitCode)
		if len(bytes.TrimSpace(raw.Stdout)) > 0 {
			run.Status = "partial"
		} else {
			run.Status = "failed"
		}
	} else {
		run.Status = "completed"
	}
	persistCtx := context.WithoutCancel(ctx)
	if err := s.store.UpdateRun(persistCtx, run); err != nil {
		logger.ErrorContext(ctx, "persist execution result failed", "error", err)
		return domain.ExecResult{}, err
	}
	s.publishExecutionEvent(ExecutionEvent{
		SessionID: run.SessionID,
		RunID:     run.ID,
		Status:    run.Status,
	})
	s.audit(persistCtx, run.ID, "command_completed", actor, map[string]any{"status": run.Status, "exit_code": run.ExitCode, "duration_ms": raw.Duration.Milliseconds()})
	completion := logger.InfoContext
	if run.Status == "failed" {
		completion = logger.ErrorContext
	}
	completion(ctx, "operation execution completed", "status", run.Status, "exit_code", run.ExitCode, "duration_ms", raw.Duration.Milliseconds(), "stdout_bytes", len(raw.Stdout), "stderr_bytes", len(raw.Stderr), "error", execErr)
	result := domain.ExecResult{
		RunID: run.ID, Status: run.Status, AutoApproved: autoApproved, ExitCode: run.ExitCode,
		Stdout: run.StdoutRedacted, Stderr: run.StderrRedacted,
		Duration: raw.Duration, Change: approvedReq.Change, Tunnel: tunnel, CompletedAt: run.CompletedAt,
		Shell: shell,
	}
	if (approvedReq.Mode == domain.ExecSSHShellStart || approvedReq.Mode == domain.ExecWorkspaceShellStart) && run.Status == "completed" {
		result.ShellUsage = sshShellUsage()
	}
	if run.Status == "completed" && (approvedReq.Mode == domain.ExecRemoteSearch || approvedReq.Mode == domain.ExecWorkspaceSearch) {
		decorateFileSearchResult(&result, approvedReq.SearchPattern, approvedReq.SearchMatchMode, approvedReq.ContextLines)
	}
	if approvedReq.Change != nil {
		path := approvedReq.RemotePath
		if approvedReq.Mode == domain.ExecWorkspaceEdit {
			path = approvedReq.RelativePath
		}
		metadata := parseFileEditOutput(path, approvedReq.Validator, result.Stdout, run.Status == "completed")
		result.File = &metadata
	}
	if approvedReq.Mode == domain.ExecWorkspaceDownload && run.Status == "completed" {
		var downloaded WorkspaceUploadResult
		if json.Unmarshal(raw.Stdout, &downloaded) == nil {
			result.File = &domain.FileMetadata{Path: downloaded.Path, Size: downloaded.Size, SHA256: downloaded.SHA256}
			result.Stdout = ""
		}
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
		if req.WorkspaceID == "" {
			return fmt.Errorf("workspace operation requires a workspace")
		}
		if req.Mode == domain.ExecWorkspaceShellStart {
			return validateInteractiveShellSize(req)
		}
		if req.Mode == domain.ExecWorkspaceSearch {
			if req.WorkspaceID == "" || req.RelativePath == "" {
				return fmt.Errorf("workspace file search requires a workspace, path, and pattern")
			}
			if err := validateFileSearchInput(req.SearchPattern, req.SearchMatchMode, req.ContextLines); err != nil {
				return fmt.Errorf("invalid Workspace file search: %w", err)
			}
		}
		if req.Mode == domain.ExecWorkspaceRead && (req.MaxBytes < 0 || req.TailLines < 0 || (req.OffsetBytes != 0 && req.TailLines != 0)) {
			return fmt.Errorf("invalid Workspace file read range")
		}
		if req.Mode == domain.ExecWorkspaceEdit && (req.RelativePath == "" || req.Change == nil || req.TextEdit == nil) {
			return fmt.Errorf("workspace file edit requires a path, generated change, and text edit")
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
	case domain.ExecRemoteEdit:
		if req.RemotePath == "" || req.Change == nil || req.TextEdit == nil {
			return fmt.Errorf("remote file edit requires a path, generated change, and text edit")
		}
	case domain.ExecWorkspaceDownload:
		if req.WorkspaceID == "" || req.RelativePath == "" {
			return fmt.Errorf("workspace download requires a workspace destination path")
		}
		if err := validateRemoteFilePath(req.RemotePath); err != nil {
			return err
		}
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(strings.ToLower(strings.TrimSpace(req.ExpectedSHA256))) {
			return fmt.Errorf("workspace download requires expected_sha256 from ssh_file_read")
		}
	case domain.ExecSSHTunnelStart:
		if err := validateSSHTunnelRequest(req); err != nil {
			return err
		}
	case domain.ExecSSHShellStart:
		if err := validateSSHShellRequest(req); err != nil {
			return err
		}
	}
	if req.Mode == domain.ExecSSHFileTransfer && req.Elevated {
		return fmt.Errorf("elevated mode is not supported for SFTP transfers")
	}
	usesSudo, err := containsShellProgram(req, "sudo")
	if err == nil && usesSudo {
		return fmt.Errorf("do not invoke sudo directly; set elevated=true and provide the underlying program")
	}
	if !req.Elevated {
		return nil
	}
	if req.Mode == domain.ExecWorkspaceUpload || req.Mode == domain.ExecWorkspaceDownload || req.Mode == domain.ExecSSHFileTransfer {
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
	if req.Mode == domain.ExecSSHTunnelStart {
		if req.Program != "" || len(req.Args) != 0 || req.Script != "" || req.Cwd != "" || len(req.Env) != 0 ||
			req.RemotePath != "" || req.SourceHostID != "" || req.SourcePath != "" || req.WorkspaceID != "" ||
			req.RelativePath != "" || req.Change != nil || req.TextEdit != nil {
			return fmt.Errorf("SSH tunnel requests cannot include command, file, transfer, or Workspace fields")
		}
	} else if sshTunnelFieldsSet(req) {
		return fmt.Errorf("SSH tunnel fields are only valid for ssh_tunnel_start requests")
	}
	if req.Mode == domain.ExecSSHShellStart {
		if req.Program != "" || len(req.Args) != 0 || req.Script != "" || len(req.Env) != 0 ||
			req.RemotePath != "" || req.SourceHostID != "" || req.SourcePath != "" || req.WorkspaceID != "" ||
			req.RelativePath != "" || req.Change != nil || req.TextEdit != nil || req.TunnelRemoteHost != "" ||
			req.TunnelRemotePort != 0 || req.TunnelLocalPort != 0 {
			return fmt.Errorf("SSH shell requests cannot include command, file, transfer, Workspace, environment, or tunnel fields")
		}
	} else if req.Mode == domain.ExecWorkspaceShellStart {
		if req.Program != "" || len(req.Args) != 0 || req.Script != "" || req.RemotePath != "" ||
			req.SourceHostID != "" || req.SourcePath != "" || req.RelativePath != "" || req.Change != nil || req.TextEdit != nil ||
			req.TunnelRemoteHost != "" || req.TunnelRemotePort != 0 || req.TunnelLocalPort != 0 || req.Elevated {
			return fmt.Errorf("Workspace shell start requests cannot include command, remote file, transfer, change, tunnel, or elevated fields")
		}
	} else if req.ShellCols != 0 || req.ShellRows != 0 {
		return fmt.Errorf("interactive shell fields are only valid for shell start requests")
	}
	if req.Mode == domain.ExecWorkspaceShell || req.Mode == domain.ExecWorkspaceShellStart {
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
	textEditTooLarge := false
	if req.TextEdit != nil {
		textEditTooLarge = len(req.TextEdit.OldText)+len(req.TextEdit.NewText) > 1<<20
		if req.Mode != domain.ExecRemoteEdit && req.Mode != domain.ExecWorkspaceEdit {
			return fmt.Errorf("text_edit is only valid for file edit requests")
		}
	}
	if len(req.Program) > 512 || len(req.Args) > 128 || len(req.Env) > 64 || len(req.Script) > 1<<20 || changeTooLarge || textEditTooLarge {
		return fmt.Errorf("execution request exceeds program, argument, environment, or 1 MiB content limits")
	}
	for _, argument := range req.Args {
		if len(argument) > 32<<10 || strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("invalid command argument")
		}
	}
	if req.Cwd != "" {
		if req.Mode == domain.ExecWorkspaceShell || req.Mode == domain.ExecWorkspaceShellStart {
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
	interactive := map[string]bool{"bash": true, "sh": true, "zsh": true, "fish": true, "su": true, "vi": true, "vim": true, "nano": true, "emacs": true, "less": true, "more": true, "man": true, "htop": true, "watch": true, "tmux": true, "screen": true}
	if interactive[program] {
		return executionToolSelectionError(req, program)
	}
	if program == "top" && !hasAnyArg(req.Args, "-b", "--batch") {
		return &ExecutionToolSelectionError{
			Message:       "ssh_exec requires top batch mode because it has no PTY",
			SuggestedTool: "ssh_exec",
			NextAction:    "for a snapshot retry ssh_exec with program=top and args=[\"-b\",\"-n\",\"1\"]; use ssh_shell only for interactive top",
			Example: map[string]any{
				"host_id": req.HostID, "program": "top", "args": []string{"-b", "-n", "1"}, "reason": req.Reason,
			},
		}
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
	req.Background = true
	host, err := s.store.GetHost(ctx, strings.TrimSpace(req.HostID))
	if err != nil {
		return domain.Task{}, err
	}
	background := s.executionCtx
	if background == nil {
		background = context.Background()
	}
	if sessionID := SessionIDFromContext(ctx); sessionID != "" {
		background = WithSessionID(background, sessionID)
	}
	if owner, ok := executionOwnerFromContext(ctx); ok {
		background = context.WithValue(background, executionOwnerContextKey{}, owner)
	}
	taskCtx, cancel := context.WithCancel(background)
	// GetHost accepts either an ID or a display name, while tasks.host_id is a
	// foreign key and must always persist the canonical ID. Keep req unchanged
	// so the execution audit still records the identifier the caller supplied.
	task := domain.Task{ID: ids.New("task"), HostID: host.ID, Status: "running", StartedAt: time.Now().UTC()}
	state := &taskState{task: task, result: domain.ExecResult{Status: "running"}, cancel: cancel, sessionID: SessionIDFromContext(ctx)}
	if err := s.store.UpsertTask(context.Background(), task, state.result, ""); err != nil {
		cancel()
		return domain.Task{}, err
	}
	s.taskMu.Lock()
	s.tasks[task.ID] = state
	s.taskMu.Unlock()
	go func() {
		result, err := s.submit(taskCtx, req, actor, func(streamName string, data []byte) {
			s.taskMu.Lock()
			if s.tasks[state.task.ID] != state {
				s.taskMu.Unlock()
				return
			}
			chunk := s.redactor.Redact(string(data))
			if streamName == "stderr" {
				state.result.Stderr += chunk
			} else {
				state.result.Stdout += chunk
			}
			taskSnapshot, resultSnapshot, taskErr := state.task, state.result, state.err
			s.taskMu.Unlock()
			_ = s.store.UpsertTask(context.Background(), taskSnapshot, resultSnapshot, taskErr)
		})
		if err == nil && result.Status == "approval_required" && result.ApprovalID != "" {
			s.taskMu.Lock()
			if s.tasks[state.task.ID] != state {
				s.taskMu.Unlock()
				_ = s.Reject(context.Background(), result.ApprovalID, "background task cancelled", actor)
				return
			}
			state.result = result
			state.task.RunID = result.RunID
			state.task.Status = "approval_required"
			state.approvalID = result.ApprovalID
			taskSnapshot, resultSnapshot := state.task, state.result
			s.taskMu.Unlock()
			_ = s.store.UpsertTask(context.Background(), taskSnapshot, resultSnapshot, "")
			notifyApproval(ctx, result)
			s.trackApprovalTask(taskCtx, state, result.ApprovalID, actor)
			return
		}
		s.taskMu.Lock()
		if s.tasks[state.task.ID] != state {
			s.taskMu.Unlock()
			return
		}
		state.result = result
		state.task.RunID = result.RunID
		state.task.EndedAt = time.Now().UTC()
		state.task.Status = result.Status
		if err != nil {
			state.err = err.Error()
			state.task.Status = "failed"
		}
		taskSnapshot, resultSnapshot, taskErr := state.task, state.result, state.err
		delete(s.tasks, state.task.ID)
		s.taskMu.Unlock()
		_ = s.store.UpsertTask(context.Background(), taskSnapshot, resultSnapshot, taskErr)
	}()
	return task, nil
}

func (s *Service) trackApprovalTask(ctx context.Context, state *taskState, approvalID, actor string) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		approval, approvalErr := s.store.GetApproval(context.Background(), approvalID)
		if approvalErr == nil {
			run, runErr := s.store.GetRun(context.Background(), approval.RunID)
			if runErr == nil {
				result := execResultFromRun(run, approval.ID, "")
				if approval.Status == "rejected" {
					result.OperatorInstruction = approval.Reason
				}
				status := run.Status
				if approval.Status == "pending" {
					status = "approval_required"
					result.Status = status
				}
				terminal := terminalExecutionStatus(status)
				s.taskMu.Lock()
				if s.tasks[state.task.ID] != state {
					s.taskMu.Unlock()
					return
				}
				changed := state.task.Status != status || terminal
				if changed {
					state.task.RunID = run.ID
					state.task.Status = status
					state.task.OperatorInstruction = result.OperatorInstruction
					state.result = result
					if terminal {
						state.task.EndedAt = time.Now().UTC()
						delete(s.tasks, state.task.ID)
					}
				}
				taskSnapshot, resultSnapshot, taskErr := state.task, state.result, state.err
				s.taskMu.Unlock()
				if changed {
					_ = s.store.UpsertTask(context.Background(), taskSnapshot, resultSnapshot, taskErr)
				}
				if terminal {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			s.taskMu.Lock()
			if s.tasks[state.task.ID] != state {
				s.taskMu.Unlock()
				return
			}
			state.task.Status = "interrupted"
			state.task.EndedAt = time.Now().UTC()
			state.result.Status = "interrupted"
			state.err = ctx.Err().Error()
			taskSnapshot, resultSnapshot, taskErr := state.task, state.result, state.err
			delete(s.tasks, state.task.ID)
			s.taskMu.Unlock()
			_ = s.store.UpsertTask(context.Background(), taskSnapshot, resultSnapshot, taskErr)
			s.audit(context.Background(), taskSnapshot.RunID, "task_interrupted", actor, map[string]any{"task_id": taskSnapshot.ID})
			return
		case <-ticker.C:
		}
	}
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

// WaitTask waits inside the control plane so callers do not need to spend one
// Tool invocation per poll. It always returns immediately for terminal tasks.
func (s *Service) WaitTask(ctx context.Context, id string, afterStdout, afterStderr int, wait time.Duration, blockUntil string) (domain.Task, domain.ExecResult, string, bool, error) {
	if wait <= 0 {
		task, result, taskErr, err := s.GetTask(id)
		return task, result, taskErr, false, err
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		task, result, taskErr, err := s.GetTask(id)
		if err != nil {
			return task, result, taskErr, false, err
		}
		terminal := terminalExecutionStatus(task.Status)
		if terminal || (blockUntil == "output" && (len(result.Stdout) > afterStdout || len(result.Stderr) > afterStderr)) {
			return task, result, taskErr, false, nil
		}
		select {
		case <-ctx.Done():
			return domain.Task{}, domain.ExecResult{}, "", false, ctx.Err()
		case <-deadline.C:
			return task, result, taskErr, true, nil
		case <-ticker.C:
		}
	}
}

func (s *Service) CancelTask(id, actor string) error {
	s.taskMu.Lock()
	state, ok := s.tasks[id]
	if !ok {
		s.taskMu.Unlock()
		if _, _, _, err := s.store.GetTask(context.Background(), id); err != nil {
			return err
		}
		return fmt.Errorf("task is not running and cannot be cancelled")
	}
	if state.task.Status != "running" && state.task.Status != "waiting_for_approval" && state.task.Status != "approval_required" {
		s.taskMu.Unlock()
		return fmt.Errorf("task is not running and cannot be cancelled")
	}
	cancel := state.cancel
	approvalID := state.approvalID
	runID := state.task.RunID
	state.task.Status = "cancelled"
	state.task.EndedAt = time.Now().UTC()
	state.result.Status = "cancelled"
	state.result.CompletedAt = state.task.EndedAt
	taskSnapshot, resultSnapshot, taskErr := state.task, state.result, state.err
	delete(s.tasks, id)
	s.taskMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if approvalID != "" {
		approval, err := s.store.GetApproval(context.Background(), approvalID)
		if err == nil && approval.Status == "pending" {
			if rejectErr := s.Reject(context.Background(), approvalID, "background task cancelled", actor); rejectErr != nil {
				s.cancelApprovedExecution(runID)
			}
		} else {
			s.cancelApprovedExecution(runID)
		}
	}
	s.audit(context.Background(), runID, "task_cancelled", actor, map[string]any{"task_id": id})
	_ = s.store.UpsertTask(context.Background(), taskSnapshot, resultSnapshot, taskErr)
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
	if sessionID := SessionIDFromContext(ctx); sessionID != "" && run.SessionID != sessionID {
		return HistoryResult{}, store.ErrNotFound
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
	return s.store.SearchRuns(ctx, query, hostID, SessionIDFromContext(ctx), limit)
}

func (s *Service) SearchRunsMatching(ctx context.Context, filter domain.RunSearchFilter, matchMode domain.FileSearchMatchMode) ([]domain.Run, error) {
	filter.SessionID = SessionIDFromContext(ctx)
	switch matchMode {
	case "", domain.FileSearchLiteral:
		return s.store.SearchRunsFiltered(ctx, filter)
	case domain.FileSearchRegex:
		return s.store.SearchRunsRegexFiltered(ctx, filter.Query, filter)
	default:
		return nil, fmt.Errorf("invalid history match_mode: use literal or regex")
	}
}

func (s *Service) SearchRunSummariesMatchingPage(ctx context.Context, filter domain.RunSearchFilter, matchMode domain.FileSearchMatchMode) (domain.RunSearchPage, error) {
	filter.SessionID = SessionIDFromContext(ctx)
	switch matchMode {
	case "", domain.FileSearchLiteral:
		return s.store.SearchRunSummariesFilteredPage(ctx, filter)
	case domain.FileSearchRegex:
		return s.store.SearchRunSummariesRegexFilteredPage(ctx, filter.Query, filter)
	default:
		return domain.RunSearchPage{}, fmt.Errorf("invalid history match_mode: use literal or regex")
	}
}

// RetryApprovalExplanation reruns the tool-free command explainer for an
// existing pending approval. It never decides the approval or
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
	settings, err := s.store.GetSystemSettings(ctx)
	if err != nil {
		return domain.Approval{}, err
	}
	if !settings.ApprovalExplanationsEnabled {
		return domain.Approval{}, fmt.Errorf("approval explanations are disabled in system settings")
	}
	reviewer := s.approvalReviewer()
	if reviewer == nil {
		return domain.Approval{}, fmt.Errorf("approval Agent is unavailable for the active model")
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

	currentTask := ""
	if approval.SessionID != "" {
		if tasks, taskErr := s.store.ListAgentTasks(ctx, approval.SessionID); taskErr == nil {
			currentTask = currentAgentTask(tasks)
		}
	}
	input := domain.CommandReviewInput{
		Request: req, Host: domain.HostCapability{ID: host.ID, Name: host.Name, AuthType: host.AuthType, SudoMode: host.SudoMode},
		CurrentTask: currentTask, RequestDigest: digest,
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

	logger.InfoContext(ctx, "approval explanation retry started", "run_id", run.ID)
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
	if freshReviewer, ok := reviewer.(FreshApprovalReviewer); ok {
		review, reviewErr = freshReviewer.ReviewFresh(explanationCtx, input)
	} else {
		review, reviewErr = reviewer.Review(explanationCtx, input)
	}
	cancel()
	if retryCtx.Err() != nil {
		return domain.Approval{}, retryCtx.Err()
	}
	review = s.normalizeCommandReview(review, reviewErr, timeoutSeconds)
	reviewJSON, err := json.Marshal(review)
	if err != nil {
		return domain.Approval{}, err
	}
	if err := s.store.UpdatePendingApprovalExplanation(retryCtx, approval.ID, run.ID, string(reviewJSON)); err != nil {
		return domain.Approval{}, err
	}
	s.audit(ctx, run.ID, "command_ai_explanation_retried", actor, map[string]any{
		"approval_id": approval.ID, "status": review.Status,
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
	}
	return approvals, nil
}

func (s *Service) ListAudit(ctx context.Context, runID string, limit int) ([]domain.AuditEvent, error) {
	return s.store.ListAudit(ctx, runID, limit)
}

func (s *Service) ListAuditPage(ctx context.Context, runID string, limit int, cursorCreated time.Time, cursorID string) (domain.AuditEventPage, error) {
	return s.store.ListAuditPage(ctx, runID, limit, cursorCreated, cursorID)
}

func (s *Service) DeleteAuditRuns(ctx context.Context, sessionID *string, actor string) (domain.AuditRunDeleteResult, error) {
	return s.store.DeleteAuditRuns(ctx, sessionID, actor)
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
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.store.AppendAudit(persistCtx, domain.AuditEvent{RunID: runID, Type: eventType, Actor: actor, Data: data}); err != nil {
		observability.FromContext(ctx).ErrorContext(context.WithoutCancel(ctx), "persist audit event failed",
			"component", "audit", "run_id", runID, "event_type", eventType, "actor", actor, "error", err)
	}
}

func normalizeRequest(req *domain.ExecRequest, limits config.Limits) {
	if req.Mode == "" {
		if req.Script != "" {
			req.Mode = domain.ExecScript
		} else {
			req.Mode = domain.ExecProgram
		}
	}
	interactiveShell := req.Mode == domain.ExecSSHShellStart || req.Mode == domain.ExecWorkspaceShellStart
	if req.TimeoutSeconds <= 0 && !interactiveShell {
		if req.Background && limits.MaxTimeoutSeconds > 0 {
			req.TimeoutSeconds = limits.MaxTimeoutSeconds
		} else {
			req.TimeoutSeconds = limits.SyncTimeoutSeconds
		}
	}
	if req.TimeoutSeconds > limits.MaxTimeoutSeconds {
		req.TimeoutSeconds = limits.MaxTimeoutSeconds
	}
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	if req.Mode == domain.ExecSSHTunnelStart {
		req.TunnelDirection = domain.SSHTunnelDirection(strings.ToLower(strings.TrimSpace(string(req.TunnelDirection))))
		if req.TunnelDirection == "" {
			req.TunnelDirection = domain.SSHTunnelDirectionLocal
		}
		req.TunnelLocalHost = strings.Trim(strings.TrimSpace(req.TunnelLocalHost), "[]")
		if req.TunnelLocalHost == "" {
			req.TunnelLocalHost = sshTunnelDefaultHost
		}
		req.TunnelRemoteHost = strings.Trim(strings.TrimSpace(req.TunnelRemoteHost), "[]")
		if req.TunnelRemoteHost == "" {
			req.TunnelRemoteHost = sshTunnelDefaultHost
		}
	}
	if req.Mode == domain.ExecSSHShellStart || req.Mode == domain.ExecWorkspaceShellStart {
		if req.ShellCols == 0 {
			req.ShellCols = 120
		}
		if req.ShellRows == 0 {
			req.ShellRows = 32
		}
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

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
