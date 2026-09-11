package service

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/security"
	"github.com/Enterpr1se0/opsnerva/internal/skills"
	"github.com/Enterpr1se0/opsnerva/internal/sshx"
	"github.com/Enterpr1se0/opsnerva/internal/store"
	"github.com/Enterpr1se0/opsnerva/internal/websearch"
	"golang.org/x/sync/singleflight"
)

const webOperatorReason = "started directly by the operator from the Web console"

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
	approvalTasks           map[string]*taskState
	taskSubscribers         map[string]map[uint64]*taskSubscriber
	taskSubscriberID        uint64
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
	stateEventMu            sync.RWMutex
	stateSubscribers        map[uint64]*stateSubscriber
	stateSubscriberID       uint64
	sftpDeletionMu          sync.Mutex
	sftpDeletions           map[string]*sftpDeletionState
	sftpDeletionSequence    uint64
	unsubscribeStoreChanges func()
	tunnelMu                sync.RWMutex
	tunnels                 map[string]*sshTunnelState
	shellMu                 sync.RWMutex
	shells                  map[string]*sshShellState
	hostShellProbes         singleflight.Group
	webSearch               *websearch.Client
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
		globalSem:            make(chan struct{}, global), hostSems: make(map[string]chan struct{}), tasks: make(map[string]*taskState), approvalTasks: make(map[string]*taskState), taskSubscribers: make(map[string]map[uint64]*taskSubscriber), workspaces: make(map[string]config.Workspace), validators: make(map[string]config.Validator), mcpRuntime: make(map[string]*mcpRuntimeState),
		mcpOAuthFlows: make(map[string]*mcpOAuthFlow), mcpOAuthByServer: make(map[string]*mcpOAuthFlow),
		mcpActivitySubscribers: make(map[uint64]*mcpActivitySubscriber),
		modelMetadata:          newModelMetadataCache(modelsDevMetadataURL),
		explanationActive:      make(map[string]*approvalExplanationTask), explanationSem: make(chan struct{}, maxConcurrentApprovalExplanations), explanationSlots: make(chan struct{}, maxQueuedApprovalExplanations),
		automaticApprovalSem: make(chan struct{}, maxConcurrentApprovalExplanations),
		executionCtx:         executionCtx, executionCancel: executionCancel,
		executionCancels: make(map[string]context.CancelFunc), cancelledExecutions: make(map[string]struct{}),
		tunnels:   make(map[string]*sshTunnelState),
		shells:    make(map[string]*sshShellState),
		webSearch: websearch.New(redactor),
	}
	if len(runtimeConfig) > 0 {
		result.dataDir = runtimeConfig[0].DataDir
		result.workspaceSandboxPath = runtimeConfig[0].WorkspaceSandboxPath
		result.skills = skills.NewRegistry(filepath.Join(result.dataDir, "skills"))
		for _, validator := range runtimeConfig[0].Validators {
			result.validators[validator.ID] = validator
		}
	}
	result.unsubscribeStoreChanges = st.SubscribeChanges(result.publishStoreChange)
	return result
}

func (s *Service) Store() *store.Store { return s.store }

func (s *Service) Shutdown(ctx context.Context) error {
	s.executionMu.Lock()
	if !s.executionClosed {
		s.executionClosed = true
		s.executionCancel()
		if s.unsubscribeStoreChanges != nil {
			s.unsubscribeStoreChanges()
			s.unsubscribeStoreChanges = nil
		}
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
