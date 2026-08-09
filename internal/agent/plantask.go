package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"eino-ops-agent/internal/service"
	"eino-ops-agent/internal/store"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/adk/middlewares/plantask"
	"github.com/cloudwego/eino/components/tool"
)

const agentTaskBaseDir = "agent-tasks"

var agentTaskToolNames = map[string]struct{}{
	plantask.TaskCreateToolName: {},
	plantask.TaskGetToolName:    {},
	plantask.TaskUpdateToolName: {},
	plantask.TaskListToolName:   {},
}

var agentTaskCatalogDescriptions = map[string]string{
	plantask.TaskCreateToolName: "Create a persistent task.",
	plantask.TaskGetToolName:    "Read one task.",
	plantask.TaskUpdateToolName: "Update a task or its dependencies.",
	plantask.TaskListToolName:   "List current tasks.",
}

type sqlitePlantaskBackend struct {
	store *store.Store
}

func (b *sqlitePlantaskBackend) LsInfo(ctx context.Context, req *plantask.LsInfoRequest) ([]plantask.FileInfo, error) {
	sessionID, err := agentTaskSession(ctx)
	if err != nil {
		return nil, err
	}
	if req == nil || filepath.Clean(req.Path) != filepath.Clean(agentTaskBaseDir) {
		return nil, fmt.Errorf("invalid agent task directory")
	}
	files, err := b.store.ListAgentTaskFiles(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	result := make([]plantask.FileInfo, 0, len(files))
	for _, file := range files {
		if filepath.Dir(file.Path) != filepath.Clean(agentTaskBaseDir) {
			continue
		}
		result = append(result, plantask.FileInfo{
			Path: file.Path, Size: int64(len(file.Content)), ModifiedAt: file.UpdatedAt.Format(time.RFC3339Nano),
		})
	}
	return result, nil
}

func (b *sqlitePlantaskBackend) Read(ctx context.Context, req *plantask.ReadRequest) (*filesystem.FileContent, error) {
	sessionID, err := agentTaskSession(ctx)
	if err != nil {
		return nil, err
	}
	if req == nil || !validAgentTaskFilePath(req.FilePath) {
		return nil, fmt.Errorf("invalid agent task file")
	}
	file, err := b.store.ReadAgentTaskFile(ctx, sessionID, filepath.Clean(req.FilePath))
	if err != nil {
		return nil, err
	}
	return &filesystem.FileContent{Content: file.Content}, nil
}

func (b *sqlitePlantaskBackend) Write(ctx context.Context, req *plantask.WriteRequest) error {
	sessionID, err := agentTaskSession(ctx)
	if err != nil {
		return err
	}
	if req == nil || !validAgentTaskFilePath(req.FilePath) {
		return fmt.Errorf("invalid agent task file")
	}
	return b.store.WriteAgentTaskFile(ctx, sessionID, filepath.Clean(req.FilePath), req.Content)
}

func (b *sqlitePlantaskBackend) Delete(ctx context.Context, req *plantask.DeleteRequest) error {
	sessionID, err := agentTaskSession(ctx)
	if err != nil {
		return err
	}
	if req == nil || !validAgentTaskFilePath(req.FilePath) {
		return fmt.Errorf("invalid agent task file")
	}
	return b.store.DeleteAgentTaskFile(ctx, sessionID, filepath.Clean(req.FilePath))
}

func agentTaskSession(ctx context.Context) (string, error) {
	sessionID := service.SessionIDFromContext(ctx)
	if sessionID == "" {
		return "", fmt.Errorf("agent tasks require a session context")
	}
	return sessionID, nil
}

func validAgentTaskFilePath(filePath string) bool {
	filePath = filepath.Clean(strings.TrimSpace(filePath))
	if filePath == "" || filepath.Dir(filePath) != filepath.Clean(agentTaskBaseDir) {
		return false
	}
	name := filepath.Base(filePath)
	if name == ".highwatermark" {
		return true
	}
	id, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
	return strings.HasSuffix(name, ".json") && err == nil && id > 0
}

type configuredPlantaskMiddleware struct {
	adk.ChatModelAgentMiddleware
	enabled map[string]bool
}

func (m *configuredPlantaskMiddleware) BeforeAgent(ctx context.Context, runCtx *adk.ChatModelAgentContext) (context.Context, *adk.ChatModelAgentContext, error) {
	ctx, runCtx, err := m.ChatModelAgentMiddleware.BeforeAgent(ctx, runCtx)
	if err != nil || runCtx == nil {
		return ctx, runCtx, err
	}
	next := *runCtx
	next.Tools = make([]tool.BaseTool, 0, len(runCtx.Tools))
	for _, candidate := range runCtx.Tools {
		info, infoErr := candidate.Info(ctx)
		if infoErr != nil {
			return ctx, runCtx, infoErr
		}
		if enabled, configured := m.enabled[info.Name]; configured && !enabled {
			continue
		}
		next.Tools = append(next.Tools, candidate)
	}
	return ctx, &next, nil
}

func newPlantaskMiddleware(ctx context.Context, st *store.Store, states map[string]bool) (adk.ChatModelAgentMiddleware, []tool.BaseTool, error) {
	framework, err := plantask.New(ctx, &plantask.Config{Backend: &sqlitePlantaskBackend{store: st}, BaseDir: agentTaskBaseDir})
	if err != nil {
		return nil, nil, err
	}
	configured := &configuredPlantaskMiddleware{ChatModelAgentMiddleware: framework, enabled: states}
	_, runCtx, err := framework.BeforeAgent(ctx, &adk.ChatModelAgentContext{})
	if err != nil {
		return nil, nil, err
	}
	return configured, runCtx.Tools, nil
}

func isAgentTaskTool(name string) bool {
	_, ok := agentTaskToolNames[name]
	return ok
}
