package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"eino-ops-agent/internal/agent"
	"eino-ops-agent/internal/config"
	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/ids"
	"eino-ops-agent/internal/mcpserver"
	"eino-ops-agent/internal/observability"
	"eino-ops-agent/internal/security"
	"eino-ops-agent/internal/service"
	"eino-ops-agent/internal/skills"
	"eino-ops-agent/internal/store"
	webui "eino-ops-agent/web"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

type Server struct {
	service    *service.Service
	agent      *agent.Runtime
	chatEvents *chatEventHub
	modelTests *modelTestJobs
	mux        *http.ServeMux
	mcpHTTP    http.Handler
	options    Options
}

type Options struct {
	Version   string
	StartedAt time.Time
	Logging   config.Logging
}

func New(svc *service.Service, agentRuntime *agent.Runtime, options Options) *Server {
	if options.StartedAt.IsZero() {
		options.StartedAt = time.Now().UTC()
	}
	if strings.TrimSpace(options.Version) == "" {
		options.Version = "unknown"
	}
	s := &Server{
		service: svc, agent: agentRuntime, mux: http.NewServeMux(),
		mcpHTTP: mcpserver.New(svc, options.Version).HTTPHandler(), chatEvents: newChatEventHub(), modelTests: newModelTestJobs(), options: options,
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return requestLogMiddleware(recoverMiddleware(corsMiddleware(s.mux)), slog.Default())
}

func (s *Server) routes() {
	s.mux.HandleFunc("/mcp", s.serveMCP)
	s.mux.HandleFunc("GET /api/v1/health", s.health)
	s.mux.HandleFunc("GET /api/v1/proxies", s.listProxies)
	s.mux.HandleFunc("POST /api/v1/proxies", s.saveProxy)
	s.mux.HandleFunc("DELETE /api/v1/proxies/{id}", s.deleteProxy)
	s.mux.HandleFunc("POST /api/v1/proxies/{id}/test", s.testProxy)
	s.mux.HandleFunc("GET /api/v1/model-providers", s.listModelProviders)
	s.mux.HandleFunc("POST /api/v1/model-providers", s.saveModelProvider)
	s.mux.HandleFunc("POST /api/v1/model-providers/discover", s.discoverModels)
	s.mux.HandleFunc("POST /api/v1/model-providers/test", s.testModelConfiguration)
	s.mux.HandleFunc("GET /api/v1/model-tests/{id}", s.getModelTest)
	s.mux.HandleFunc("DELETE /api/v1/model-providers/{id}", s.deleteModelProvider)
	s.mux.HandleFunc("POST /api/v1/model-providers/{id}/activate", s.activateModelProvider)
	s.mux.HandleFunc("POST /api/v1/model-providers/{id}/test", s.testModelProvider)
	s.mux.HandleFunc("GET /api/v1/settings", s.systemSettings)
	s.mux.HandleFunc("GET /api/v1/web-search/settings", s.webSearchSettings)
	s.mux.HandleFunc("PUT /api/v1/web-search/settings", s.saveWebSearchSettings)
	s.mux.HandleFunc("POST /api/v1/web-search/test", s.testWebSearch)
	s.mux.HandleFunc("GET /api/v1/capabilities", s.capabilities)
	s.mux.HandleFunc("GET /api/v1/agent/tools", s.agentTools)
	s.mux.HandleFunc("POST /api/v1/agent/tools/{name}/enable", s.enableAgentTool)
	s.mux.HandleFunc("POST /api/v1/agent/tools/{name}/disable", s.disableAgentTool)
	s.mux.HandleFunc("GET /api/v1/skills", s.listSkills)
	s.mux.HandleFunc("POST /api/v1/skills", s.uploadSkill)
	s.mux.HandleFunc("POST /api/v1/skills/reload", s.reloadSkills)
	s.mux.HandleFunc("GET /api/v1/skills/{name}", s.getSkill)
	s.mux.HandleFunc("PUT /api/v1/skills/{name}", s.saveSkill)
	s.mux.HandleFunc("DELETE /api/v1/skills/{name}", s.deleteSkill)
	s.mux.HandleFunc("POST /api/v1/skills/{name}/enable", s.enableSkill)
	s.mux.HandleFunc("POST /api/v1/skills/{name}/disable", s.disableSkill)
	s.mux.HandleFunc("GET /api/v1/mcp-servers", s.listMCPServers)
	s.mux.HandleFunc("POST /api/v1/mcp-servers", s.saveMCPServer)
	s.mux.HandleFunc("GET /api/v1/mcp-servers/{id}", s.getMCPServer)
	s.mux.HandleFunc("PUT /api/v1/mcp-servers/{id}", s.updateMCPServer)
	s.mux.HandleFunc("DELETE /api/v1/mcp-servers/{id}", s.deleteMCPServer)
	s.mux.HandleFunc("POST /api/v1/mcp-servers/{id}/enable", s.enableMCPServer)
	s.mux.HandleFunc("POST /api/v1/mcp-servers/{id}/disable", s.disableMCPServer)
	s.mux.HandleFunc("POST /api/v1/mcp-servers/{id}/retry", s.retryMCPServer)
	s.mux.HandleFunc("POST /api/v1/mcp-servers/{id}/test", s.testMCPServer)
	s.mux.HandleFunc("POST /api/v1/mcp-servers/{id}/oauth", s.startMCPOAuth)
	s.mux.HandleFunc("DELETE /api/v1/mcp-servers/{id}/oauth", s.clearMCPOAuth)
	s.mux.HandleFunc("GET /api/v1/mcp/oauth/callback", s.completeMCPOAuth)
	s.mux.HandleFunc("POST /api/v1/workspaces", s.createWorkspace)
	s.mux.HandleFunc("PUT /api/v1/workspaces/{id}", s.updateWorkspace)
	s.mux.HandleFunc("DELETE /api/v1/workspaces/{id}", s.deleteWorkspace)
	s.mux.HandleFunc("GET /api/v1/workspaces/{id}/files", s.listWorkspaceFiles)
	s.mux.HandleFunc("POST /api/v1/workspaces/{id}/files", s.uploadWorkspaceFile)
	s.mux.HandleFunc("PUT /api/v1/workspaces/{id}/files", s.saveWorkspaceTextFile)
	s.mux.HandleFunc("DELETE /api/v1/workspaces/{id}/files", s.deleteWorkspaceEntry)
	s.mux.HandleFunc("GET /api/v1/workspaces/{id}/preview", s.previewWorkspaceFile)
	s.mux.HandleFunc("GET /api/v1/workspaces/{id}/download", s.downloadWorkspaceFile)
	s.mux.HandleFunc("GET /api/v1/workspaces/{id}/events", s.workspaceFileEvents)
	s.mux.HandleFunc("PUT /api/v1/settings", s.saveSystemSettings)
	s.mux.HandleFunc("GET /api/v1/hosts", s.listHosts)
	s.mux.HandleFunc("POST /api/v1/hosts", s.saveHost)
	s.mux.HandleFunc("GET /api/v1/hosts/{id}", s.getHost)
	s.mux.HandleFunc("DELETE /api/v1/hosts/{id}", s.deleteHost)
	s.mux.HandleFunc("POST /api/v1/hosts/{id}/scan-key", s.scanHostKey)
	s.mux.HandleFunc("POST /api/v1/hosts/{id}/trust-key", s.trustHostKey)
	s.mux.HandleFunc("POST /api/v1/hosts/{id}/probe", s.probeHost)
	s.mux.HandleFunc("GET /api/v1/hosts/{id}/sftp/entries", s.listSFTPEntries)
	s.mux.HandleFunc("PATCH /api/v1/hosts/{id}/sftp/entries", s.renameSFTPEntry)
	s.mux.HandleFunc("DELETE /api/v1/hosts/{id}/sftp/entries", s.deleteSFTPEntry)
	s.mux.HandleFunc("GET /api/v1/hosts/{id}/sftp/files", s.downloadSFTPFile)
	s.mux.HandleFunc("PUT /api/v1/hosts/{id}/sftp/files", s.uploadSFTPFile)
	s.mux.HandleFunc("POST /api/v1/hosts/{id}/sftp/directories", s.createSFTPDirectory)
	s.mux.HandleFunc("GET /api/v1/ssh-tunnels", s.listSSHTunnels)
	s.mux.HandleFunc("POST /api/v1/ssh-tunnels", s.startSSHTunnel)
	s.mux.HandleFunc("DELETE /api/v1/ssh-tunnels/{id}", s.stopSSHTunnel)
	s.mux.HandleFunc("GET /api/v1/ssh-shells", s.listSSHShells)
	s.mux.HandleFunc("POST /api/v1/ssh-shells", s.startSSHShell)
	s.mux.HandleFunc("GET /api/v1/ssh-shells/{id}", s.getSSHShell)
	s.mux.HandleFunc("GET /api/v1/ssh-shells/{id}/events", s.sshShellEvents)
	s.mux.HandleFunc("POST /api/v1/ssh-shells/{id}/input", s.sshShellInput)
	s.mux.HandleFunc("POST /api/v1/ssh-shells/{id}/resize", s.resizeSSHShell)
	s.mux.HandleFunc("POST /api/v1/ssh-shells/{id}/interrupt", s.interruptSSHShell)
	s.mux.HandleFunc("DELETE /api/v1/ssh-shells/{id}", s.closeSSHShell)
	s.mux.HandleFunc("POST /api/v1/exec", s.exec)
	s.mux.HandleFunc("POST /api/v1/tasks", s.startTask)
	s.mux.HandleFunc("GET /api/v1/tasks/{id}", s.getTask)
	s.mux.HandleFunc("POST /api/v1/tasks/{id}/cancel", s.cancelTask)
	s.mux.HandleFunc("GET /api/v1/approvals", s.listApprovals)
	s.mux.HandleFunc("POST /api/v1/approvals/{id}/explanation/retry", s.retryApprovalExplanation)
	s.mux.HandleFunc("POST /api/v1/approvals/{id}/approve", s.approve)
	s.mux.HandleFunc("POST /api/v1/approvals/{id}/reject", s.reject)
	s.mux.HandleFunc("GET /api/v1/runs", s.searchRuns)
	s.mux.HandleFunc("GET /api/v1/runs/{id}", s.getRun)
	s.mux.HandleFunc("GET /api/v1/audit", s.listAudit)
	s.mux.HandleFunc("GET /api/v1/logs", s.logs)
	s.mux.HandleFunc("GET /api/v1/logs/export", s.exportLogs)
	s.mux.HandleFunc("POST /api/v1/chat", s.chat)
	s.mux.HandleFunc("GET /api/v1/chat/sessions", s.chatSessions)
	s.mux.HandleFunc("GET /api/v1/chat/{id}/events", s.chatEventsStream)
	s.mux.HandleFunc("POST /api/v1/chat/{id}/cancel", s.cancelChatSession)
	s.mux.HandleFunc("PUT /api/v1/chat/{id}/workspace", s.setChatSessionWorkspace)
	s.mux.HandleFunc("DELETE /api/v1/chat/{id}", s.deleteChatSession)
	s.mux.HandleFunc("GET /api/v1/chat/{id}/messages", s.chatMessages)
	s.mux.HandleFunc("GET /api/v1/chat/{id}/state", s.chatState)
	s.mux.HandleFunc("GET /api/v1/chat/{id}/attachments/{attachment_id}", s.chatAttachment)
	s.mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, _ *http.Request) {
		writeErrorStatus(w, fmt.Errorf("API endpoint not found"), http.StatusNotFound)
	})
	s.mux.Handle("/", spaHandler(webui.Assets()))
}

func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	token := ""
	if scheme, value, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " "); ok && strings.EqualFold(scheme, "Bearer") {
		token = strings.TrimSpace(value)
	}
	enabled, authorized, err := s.service.MCPHTTPAccess(r.Context(), token)
	if err != nil {
		writeError(w, err)
		return
	}
	if !enabled {
		writeErrorStatus(w, fmt.Errorf("MCP HTTP server is disabled"), http.StatusNotFound)
		return
	}
	if !authorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeErrorStatus(w, fmt.Errorf("invalid MCP HTTP token"), http.StatusUnauthorized)
		return
	}
	s.mcpHTTP.ServeHTTP(w, r)
}

func (s *Server) capabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": s.service.ListAdminWorkspaceCapabilities()})
}

func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var input domain.WorkspaceInput
	if !decode(w, r, &input) {
		return
	}
	result, err := s.service.CreateAdminWorkspace(r.Context(), input, actor(r))
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) updateWorkspace(w http.ResponseWriter, r *http.Request) {
	var input domain.WorkspaceInput
	if !decode(w, r, &input) {
		return
	}
	result, err := s.service.UpdateAdminWorkspace(r.Context(), r.PathValue("id"), input, actor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := s.service.DeleteAdminWorkspace(r.Context(), r.PathValue("id"), actor(r)); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) agentTools(w http.ResponseWriter, _ *http.Request) {
	if s.agent == nil {
		writeJSON(w, http.StatusOK, agent.ToolCatalog{Agent: "ops-nerva", Framework: "Eino InferTool", ExecutionMode: "sequential", Tools: []agent.ToolDescriptor{}})
		return
	}
	writeJSON(w, http.StatusOK, s.agent.ToolCatalog())
}

func (s *Server) enableAgentTool(w http.ResponseWriter, r *http.Request) {
	s.setAgentToolEnabled(w, r, true)
}

func (s *Server) disableAgentTool(w http.ResponseWriter, r *http.Request) {
	s.setAgentToolEnabled(w, r, false)
}

func (s *Server) setAgentToolEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	name := r.PathValue("name")
	if s.agent == nil || !s.agent.HasTool(name) {
		writeErrorStatus(w, fmt.Errorf("agent function %q not found", name), http.StatusNotFound)
		return
	}
	if err := s.service.SetAgentToolEnabled(r.Context(), name, enabled, actor(r)); err != nil {
		writeErrorStatus(w, err, http.StatusBadRequest)
		return
	}
	if err := s.agent.Reload(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.agent.ToolCatalog())
}

func (s *Server) listSkills(w http.ResponseWriter, _ *http.Request) {
	result, err := s.service.ListSkills()
	respond(w, result, err)
}

func (s *Server) reloadSkills(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		writeErrorStatus(w, agent.ErrUnavailable, http.StatusServiceUnavailable)
		return
	}
	if err := s.agent.Reload(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.agent.ToolCatalog())
}

func (s *Server) getSkill(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.GetAdminSkill(r.PathValue("name"))
	if errors.Is(err, skills.ErrNotFound) {
		writeErrorStatus(w, err, http.StatusNotFound)
		return
	}
	respond(w, result, err)
}

func (s *Server) uploadSkill(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 9<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeErrorStatus(w, fmt.Errorf("invalid skill upload: %w", err), status)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErrorStatus(w, fmt.Errorf("skill file is required"), http.StatusBadRequest)
		return
	}
	defer file.Close()
	name := strings.TrimSpace(r.FormValue("name"))
	result, err := s.service.ImportAdminSkills(r.Context(), name, header.Filename, file, actor(r))
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) saveSkill(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, (2<<20)+(16<<10))
	var input service.SkillContentInput
	if !decode(w, r, &input) {
		return
	}
	result, err := s.service.SaveAdminSkill(r.Context(), r.PathValue("name"), input.Content, actor(r))
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) deleteSkill(w http.ResponseWriter, r *http.Request) {
	err := s.service.DeleteAdminSkill(r.Context(), r.PathValue("name"), actor(r))
	if errors.Is(err, skills.ErrNotFound) {
		writeErrorStatus(w, err, http.StatusNotFound)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) enableSkill(w http.ResponseWriter, r *http.Request) {
	s.setSkillEnabled(w, r, true)
}

func (s *Server) disableSkill(w http.ResponseWriter, r *http.Request) {
	s.setSkillEnabled(w, r, false)
}

func (s *Server) setSkillEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	result, err := s.service.SetAdminSkillEnabled(r.Context(), r.PathValue("name"), enabled, actor(r))
	if errors.Is(err, skills.ErrNotFound) {
		writeErrorStatus(w, err, http.StatusNotFound)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if s.agent != nil {
		if err := s.agent.Reload(r.Context()); err != nil {
			writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listMCPServers(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.ListMCPServers(r.Context())
	respond(w, result, err)
}

func (s *Server) getMCPServer(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.GetMCPServer(r.Context(), r.PathValue("id"))
	respond(w, result, err)
}

func (s *Server) saveMCPServer(w http.ResponseWriter, r *http.Request) {
	s.saveMCPServerInput(w, r, "", http.StatusCreated)
}

func (s *Server) updateMCPServer(w http.ResponseWriter, r *http.Request) {
	s.saveMCPServerInput(w, r, r.PathValue("id"), http.StatusOK)
}

func (s *Server) saveMCPServerInput(w http.ResponseWriter, r *http.Request, id string, status int) {
	var input domain.MCPServerInput
	if !decode(w, r, &input) {
		return
	}
	if id != "" {
		input.ID = id
	}
	result, err := s.service.SaveMCPServer(r.Context(), input, actor(r))
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadRequest)
		return
	}
	if err := s.reloadAgent(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, status, result)
}

func (s *Server) deleteMCPServer(w http.ResponseWriter, r *http.Request) {
	if err := s.service.DeleteMCPServer(r.Context(), r.PathValue("id"), actor(r)); err != nil {
		writeError(w, err)
		return
	}
	if err := s.reloadAgent(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) enableMCPServer(w http.ResponseWriter, r *http.Request) {
	s.setMCPServerEnabled(w, r, true)
}

func (s *Server) disableMCPServer(w http.ResponseWriter, r *http.Request) {
	s.setMCPServerEnabled(w, r, false)
}

func (s *Server) setMCPServerEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	result, err := s.service.SetMCPServerEnabled(r.Context(), r.PathValue("id"), enabled, actor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.reloadAgent(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) retryMCPServer(w http.ResponseWriter, r *http.Request) {
	err := s.service.ReconnectMCPServer(r.Context(), r.PathValue("id"))
	reloadErr := s.reloadAgent(r.Context())
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadGateway)
		return
	}
	if reloadErr != nil {
		writeError(w, reloadErr)
		return
	}
	result, err := s.service.GetMCPServer(r.Context(), r.PathValue("id"))
	respond(w, result, err)
}

func (s *Server) testMCPServer(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.TestMCPServer(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) startMCPOAuth(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	redirectURL := scheme + "://" + r.Host + "/api/v1/mcp/oauth/callback"
	result, err := s.service.BeginMCPOAuth(r.Context(), r.PathValue("id"), redirectURL, actor(r))
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) clearMCPOAuth(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.ClearMCPOAuth(r.Context(), r.PathValue("id"), actor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.reloadAgent(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) completeMCPOAuth(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	authorizationError := query.Get("error_description")
	if authorizationError == "" {
		authorizationError = query.Get("error")
	}
	err := s.service.CompleteMCPOAuth(r.Context(), query.Get("state"), query.Get("code"), query.Get("iss"), authorizationError)
	if err == nil {
		err = s.reloadAgent(r.Context())
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>授权失败</title><body>授权失败，可以关闭此窗口。</body></html>`)
		return
	}
	_, _ = io.WriteString(w, `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>授权完成</title><body>授权完成，可以关闭此窗口。<script>window.close()</script></body></html>`)
}

func (s *Server) reloadAgent(ctx context.Context) error {
	if s.agent == nil {
		return nil
	}
	return s.agent.Reload(ctx)
}

func (s *Server) listWorkspaceFiles(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.ListAdminWorkspaceFiles(r.PathValue("id"), r.URL.Query().Get("path"))
	respond(w, result, err)
}

func (s *Server) previewWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.PreviewAdminWorkspaceFile(r.PathValue("id"), r.URL.Query().Get("path"))
	respond(w, result, err)
}

func (s *Server) downloadWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.OpenAdminWorkspaceFile(r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, err)
		return
	}
	defer result.Reader.Close()
	contentType := mime.TypeByExtension(filepath.Ext(result.Name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": result.Name}))
	w.Header().Set("Content-Length", strconv.FormatInt(result.Size, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(w, result.Reader); err != nil {
		observability.FromContext(r.Context()).ErrorContext(r.Context(), "Workspace download failed", "component", "server", "workspace_id", result.WorkspaceID, "path", result.Path, "error", err)
	}
}

func (s *Server) saveWorkspaceTextFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, (100<<20)+(1<<20))
	var input struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if !decode(w, r, &input) {
		return
	}
	result, err := s.service.SaveAdminWorkspaceTextFile(r.Context(), r.PathValue("id"), input.Path, input.Content)
	respond(w, result, err)
}

func (s *Server) deleteWorkspaceEntry(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.DeleteAdminWorkspaceEntry(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"), actor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) uploadWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.UploadWorkspaceFile(
		r.Context(),
		r.PathValue("id"),
		r.URL.Query().Get("path"),
		r.URL.Query().Get("filename"),
		r.Body,
		actor(r),
	)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeErrorStatus(w, err, status)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) workspaceFileEvents(w http.ResponseWriter, r *http.Request) {
	watch, err := s.service.WatchAdminWorkspaceFiles(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, err)
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, fmt.Errorf("streaming is unavailable"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	controller := http.NewResponseController(w)
	write := func(payload string) error {
		if _, err := io.WriteString(w, payload); err != nil {
			return err
		}
		return controller.Flush()
	}
	if err := write("retry: 1000\n: connected\n\n"); err != nil {
		return
	}

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case change, open := <-watch.Changes:
			if !open {
				return
			}
			payload, marshalErr := json.Marshal(change)
			if marshalErr != nil {
				return
			}
			if err := write("event: workspace-change\ndata: " + string(payload) + "\n\n"); err != nil {
				return
			}
		case watchErr, open := <-watch.Errors:
			if !open {
				return
			}
			observability.FromContext(r.Context()).WarnContext(r.Context(), "workspace file watcher stopped", "component", "workspace", "workspace_id", r.PathValue("id"), "error", watchErr)
			return
		case <-heartbeat.C:
			if err := write(": heartbeat\n\n"); err != nil {
				return
			}
		}
	}
}

func (s *Server) listSSHShells(w http.ResponseWriter, r *http.Request) {
	activeOnly := !strings.EqualFold(r.URL.Query().Get("active"), "false")
	result, err := s.service.ListSSHShells(r.Context(), r.URL.Query().Get("session_id"), activeOnly, r.URL.Query().Get("reason"), actor(r))
	respond(w, result, err)
}

func (s *Server) startSSHShell(w http.ResponseWriter, r *http.Request) {
	var input struct {
		HostID      string `json:"host_id"`
		WorkspaceID string `json:"workspace_id"`
		Cwd         string `json:"cwd"`
		Surface     string `json:"surface"`
	}
	if !decode(w, r, &input) {
		return
	}
	var result domain.SSHShell
	var err error
	if strings.TrimSpace(input.WorkspaceID) != "" {
		if strings.TrimSpace(input.HostID) != "" || strings.TrimSpace(input.Surface) != "" {
			writeSSHShellError(w, fmt.Errorf("workspace_id cannot be combined with host_id or surface"))
			return
		}
		result, err = s.service.StartOperatorWorkspaceShell(r.Context(), input.WorkspaceID, input.Cwd, actor(r))
	} else {
		result, err = s.service.StartOperatorSSHShell(r.Context(), input.HostID, input.Surface, actor(r))
	}
	if err != nil {
		writeSSHShellError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) getSSHShell(w http.ResponseWriter, r *http.Request) {
	after, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	if r.URL.Query().Get("after") == "" {
		after, err = 0, nil
	}
	if err != nil {
		writeErrorStatus(w, fmt.Errorf("after must be a non-negative event sequence"), http.StatusBadRequest)
		return
	}
	coalesce := strings.EqualFold(r.URL.Query().Get("coalesce"), "true")
	result, err := s.service.GetSSHShellSnapshot(r.Context(), r.PathValue("id"), "", after, 0, coalesce, r.URL.Query().Get("reason"), actor(r))
	if errors.Is(err, store.ErrNotFound) {
		writeErrorStatus(w, err, http.StatusNotFound)
		return
	}
	respond(w, result, err)
}

func (s *Server) sshShellInput(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Input     string `json:"input"`
		Sensitive bool   `json:"sensitive"`
		Submit    bool   `json:"submit"`
		Reason    string `json:"reason"`
	}
	if !decodeLimit(w, r, &input, (64<<10)+(1<<10)) {
		return
	}
	if input.Sensitive {
		if input.Submit && !strings.HasSuffix(input.Input, "\r") && !strings.HasSuffix(input.Input, "\n") {
			input.Input += "\r"
		}
		if err := s.service.WriteSensitiveSSHShellInput(r.Context(), r.PathValue("id"), input.Input, actor(r)); err != nil {
			writeSSHShellError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if input.Submit && !strings.HasSuffix(input.Input, "\r") && !strings.HasSuffix(input.Input, "\n") {
		input.Input += "\r"
	}
	if err := s.service.SendSSHShellInput(r.Context(), r.PathValue("id"), "", input.Input, input.Reason, actor(r)); err != nil {
		writeSSHShellError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resizeSSHShell(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	if !decode(w, r, &input) {
		return
	}
	result, err := s.service.ResizeSSHShell(r.Context(), r.PathValue("id"), input.Cols, input.Rows, actor(r))
	if err != nil {
		writeSSHShellError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) interruptSSHShell(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &input) {
		return
	}
	result, err := s.service.InterruptSSHShell(r.Context(), r.PathValue("id"), "", input.Reason, actor(r))
	if err != nil {
		writeSSHShellError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) closeSSHShell(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.CloseSSHShell(r.Context(), r.PathValue("id"), "", r.URL.Query().Get("reason"), actor(r))
	if err != nil {
		writeSSHShellError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) sshShellEvents(w http.ResponseWriter, r *http.Request) {
	after, err := sshShellEventSequence(r)
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadRequest)
		return
	}
	initial, err := s.service.GetSSHShellSnapshot(r.Context(), r.PathValue("id"), "", after, 0, false, "", "")
	if errors.Is(err, store.ErrNotFound) {
		writeErrorStatus(w, err, http.StatusNotFound)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, fmt.Errorf("streaming is unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	controller := http.NewResponseController(w)
	write := func(payload string) error {
		if _, err := io.WriteString(w, payload); err != nil {
			return err
		}
		return controller.Flush()
	}
	if err := write("retry: 1000\n: connected\n\n"); err != nil {
		return
	}
	snapshot := initial
	for {
		for _, event := range snapshot.Events {
			data, marshalErr := json.Marshal(event)
			if marshalErr != nil {
				return
			}
			if err := write(fmt.Sprintf("id: %d\nevent: shell-event\ndata: %s\n\n", event.Sequence, data)); err != nil {
				return
			}
			after = event.Sequence
		}
		if !shellHTTPStatusActive(snapshot.Shell.Status) {
			return
		}
		snapshot, err = s.service.GetSSHShellSnapshot(r.Context(), r.PathValue("id"), "", after, 10*time.Second, false, "", "")
		if err != nil {
			return
		}
		if len(snapshot.Events) == 0 {
			if err := write(": heartbeat\n\n"); err != nil {
				return
			}
		}
	}
}

func sshShellEventSequence(r *http.Request) (uint64, error) {
	afterText := r.Header.Get("Last-Event-ID")
	if afterText == "" {
		afterText = r.URL.Query().Get("after")
	}
	after := uint64(0)
	if afterText != "" {
		value, err := strconv.ParseUint(afterText, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("after must be a non-negative event sequence")
		}
		after = value
	}
	return after, nil
}

func writeSSHShellError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, store.ErrNotFound) {
		status = http.StatusNotFound
	} else if strings.Contains(err.Error(), "is running") || strings.Contains(err.Error(), "limit reached") {
		status = http.StatusConflict
	}
	writeErrorStatus(w, err, status)
}

func shellHTTPStatusActive(status string) bool {
	switch status {
	case "starting", "running", "stopping":
		return true
	default:
		return false
	}
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	result := observability.Recent(observability.LogFilter{
		Level: r.URL.Query().Get("level"), Component: r.URL.Query().Get("component"),
		Query: r.URL.Query().Get("q"), Limit: limit,
	})
	writeJSON(w, http.StatusOK, map[string]any{"entries": result, "components": observability.Components(), "minimum_level": observability.MinimumLevel(), "file": observability.File()})
}

func (s *Server) exportLogs(w http.ResponseWriter, r *http.Request) {
	filename := "opsnerva-diagnostics-" + time.Now().UTC().Format("20060102-150405") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Cache-Control", "no-store")
	if err := observability.WriteArchive(w, s.diagnostics(r.Context())); err != nil {
		observability.FromContext(r.Context()).ErrorContext(r.Context(), "log export failed", "component", "server", "error", err)
	}
}

func (s *Server) diagnostics(ctx context.Context) observability.Diagnostics {
	now := time.Now().UTC()
	uptime := now.Sub(s.options.StartedAt).Seconds()
	if uptime < 0 {
		uptime = 0
	}
	result := observability.Diagnostics{
		SchemaVersion: 1,
		GeneratedAt:   now,
		Application: observability.ApplicationDiagnostics{
			Version: s.options.Version, GoVersion: runtime.Version(), OS: runtime.GOOS, Architecture: runtime.GOARCH,
			StartedAt: s.options.StartedAt.UTC(), UptimeSeconds: int64(uptime),
		},
		Logging: observability.LoggingDiagnostics{
			Level: observability.MinimumLevel(), Format: s.options.Logging.Format, FileEnabled: observability.File() != "",
			AddSource: s.options.Logging.AddSource, MaxSizeMB: s.options.Logging.MaxSizeMB,
			MaxBackups: s.options.Logging.MaxBackups, RecentLimit: s.options.Logging.RecentLimit,
		},
		Resources: observability.ResourceDiagnostics{MCPStatuses: map[string]int{}},
	}
	if result.Logging.Format == "" {
		result.Logging.Format = "text"
	}
	redactor := security.NewRedactor()
	addError := func(component string, err error) {
		if err != nil {
			result.CollectionErrors = append(result.CollectionErrors, component+": "+redactor.Redact(err.Error()))
		}
	}
	if s.agent != nil {
		status := s.agent.Status()
		catalog := s.agent.ToolCatalog()
		result.Agent = observability.AgentDiagnostics{
			Available: s.agent.Available(), Source: status.Source, ProviderName: redactor.Redact(status.Name), Model: redactor.Redact(status.Model),
			ToolCount: len(catalog.Tools), ApprovalAgentAvailable: status.ApprovalAgentAvailable,
			AutomaticApprovalAgentAvailable: status.AutomaticApprovalAgentAvailable,
			ModelError:                      redactor.Redact(status.Error), ApprovalError: redactor.Redact(status.ApprovalError),
			AutomaticApprovalError: redactor.Redact(status.AutomaticApprovalError),
		}
	} else {
		result.Agent.Source = "none"
	}
	if s.service == nil {
		return result
	}
	settings, err := s.service.SystemSettings(ctx)
	addError("system_settings", err)
	result.Agent.ApprovalMode = settings.ApprovalMode
	hosts, err := s.service.ListHosts(ctx)
	addError("hosts", err)
	result.Resources.Hosts = len(hosts)
	proxies, err := s.service.ListProxies(ctx)
	addError("proxies", err)
	result.Resources.Proxies = len(proxies)
	providers, err := s.service.ListModelProviders(ctx)
	addError("model_providers", err)
	result.Resources.ModelProviders = len(providers)
	for _, provider := range providers {
		if provider.Active {
			result.Resources.ActiveProviders++
		}
	}
	mcpServers, err := s.service.ListMCPServers(ctx)
	addError("mcp_servers", err)
	result.Resources.MCPServers = len(mcpServers)
	for _, server := range mcpServers {
		status := strings.TrimSpace(server.Status)
		if status == "" {
			status = "unknown"
		}
		result.Resources.MCPStatuses[status]++
	}
	workspaces := s.service.ListWorkspaceCapabilities()
	result.Resources.Workspaces = len(workspaces)
	for _, workspace := range workspaces {
		if workspace.Access == "read_write" {
			result.Resources.WritableWorkspaces++
		}
	}
	managedSkills, err := s.service.ListSkills()
	addError("skills", err)
	result.Resources.Skills = len(managedSkills)
	for _, managedSkill := range managedSkills {
		if managedSkill.Enabled {
			result.Resources.EnabledSkills++
		}
	}
	return result
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	model := agent.Status{Source: "none"}
	if s.agent != nil {
		model = s.agent.Status()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "agent_available": model.Available, "model": model, "time": time.Now().UTC(),
	})
}

func (s *Server) systemSettings(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.SystemSettings(r.Context())
	respond(w, result, err)
}

func (s *Server) saveSystemSettings(w http.ResponseWriter, r *http.Request) {
	var input domain.SystemSettingsInput
	if !decode(w, r, &input) {
		return
	}
	result, err := s.service.SaveSystemSettings(r.Context(), input, actor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	if s.agent != nil {
		if err := s.agent.Reload(r.Context()); err != nil {
			writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) webSearchSettings(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.WebSearchSettings(r.Context())
	respond(w, result, err)
}

func (s *Server) listProxies(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.ListProxies(r.Context())
	respond(w, result, err)
}

func (s *Server) saveProxy(w http.ResponseWriter, r *http.Request) {
	var input domain.ProxyInput
	if !decode(w, r, &input) {
		return
	}
	result, err := s.service.SaveProxy(r.Context(), input, actor(r))
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadRequest)
		return
	}
	if s.agent != nil {
		if err := s.agent.Reload(r.Context()); err != nil {
			writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) deleteProxy(w http.ResponseWriter, r *http.Request) {
	err := s.service.DeleteProxy(r.Context(), r.PathValue("id"), actor(r))
	if errors.Is(err, service.ErrProxyInUse) {
		writeErrorStatus(w, err, http.StatusConflict)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testProxy(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.TestProxy(r.Context(), r.PathValue("id"), actor(r))
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) saveWebSearchSettings(w http.ResponseWriter, r *http.Request) {
	var input domain.WebSearchSettingsInput
	if !decode(w, r, &input) {
		return
	}
	result, err := s.service.SaveWebSearchSettings(r.Context(), input, actor(r))
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadRequest)
		return
	}
	if s.agent != nil {
		if err := s.agent.Reload(r.Context()); err != nil {
			writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) testWebSearch(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Query string `json:"query"`
	}
	if !decode(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Query) == "" {
		input.Query = "Tavily Search API"
	}
	result, err := s.service.SearchWeb(r.Context(), domain.WebSearchRequest{Query: input.Query, MaxResults: 1}, actor(r))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		} else if errors.Is(err, service.ErrWebSearchUpstream) {
			status = http.StatusBadGateway
		}
		writeErrorStatus(w, err, status)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listModelProviders(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.ListModelProviders(r.Context())
	respond(w, result, err)
}

func (s *Server) saveModelProvider(w http.ResponseWriter, r *http.Request) {
	var input domain.ModelProviderInput
	if !decode(w, r, &input) {
		return
	}
	result, err := s.service.SaveModelProvider(r.Context(), input, actor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	settings, err := s.service.SystemSettings(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if (result.Active || settings.SubagentModelProviderID == result.ID || settings.AutomaticApprovalModelProviderID == result.ID) && s.agent != nil {
		if err := s.agent.Reload(r.Context()); err != nil {
			writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) discoverModels(w http.ResponseWriter, r *http.Request) {
	var input domain.ModelDiscoveryInput
	if !decode(w, r, &input) {
		return
	}
	result, err := s.service.DiscoverModels(r.Context(), input, actor(r))
	if err != nil {
		if errors.Is(err, service.ErrModelProviderUpstream) {
			writeErrorStatus(w, err, http.StatusBadGateway)
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) testModelConfiguration(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		writeErrorStatus(w, agent.ErrUnavailable, http.StatusServiceUnavailable)
		return
	}
	var input domain.ModelTestInput
	if !decode(w, r, &input) {
		return
	}
	cfg, err := s.service.ModelTestConfig(r.Context(), input)
	if err != nil {
		writeError(w, err)
		return
	}
	job := s.modelTests.start(context.WithoutCancel(r.Context()), cfg, modelTestIdentity{}, s.agent.TestProvider)
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) activateModelProvider(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.ActivateModelProvider(r.Context(), r.PathValue("id"), actor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	if s.agent == nil {
		writeError(w, agent.ErrUnavailable)
		return
	}
	if err := s.agent.Reload(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) deleteModelProvider(w http.ResponseWriter, r *http.Request) {
	wasActive, err := s.service.DeleteModelProvider(r.Context(), r.PathValue("id"), actor(r))
	if err != nil {
		if errors.Is(err, service.ErrModelProviderInUse) {
			writeErrorStatus(w, err, http.StatusConflict)
			return
		}
		writeError(w, err)
		return
	}
	if wasActive && s.agent != nil {
		if err := s.agent.Reload(r.Context()); err != nil {
			writeError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testModelProvider(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		writeErrorStatus(w, agent.ErrUnavailable, http.StatusServiceUnavailable)
		return
	}
	cfg, provider, err := s.service.ModelProviderConfig(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	job := s.modelTests.start(context.WithoutCancel(r.Context()), cfg, modelTestIdentity{ProviderID: provider.ID, Name: provider.Name}, s.agent.TestProvider)
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) getModelTest(w http.ResponseWriter, r *http.Request) {
	if s.modelTests == nil {
		writeErrorStatus(w, store.ErrNotFound, http.StatusNotFound)
		return
	}
	job, ok := s.modelTests.get(r.PathValue("id"))
	if !ok {
		writeErrorStatus(w, store.ErrNotFound, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) listHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.service.ListHosts(r.Context())
	respond(w, hosts, err)
}

func (s *Server) listSSHTunnels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.service.ListSSHTunnels())
}

func (s *Server) startSSHTunnel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		HostID     string `json:"host_id"`
		RemoteHost string `json:"remote_host"`
		RemotePort int    `json:"remote_port"`
		LocalPort  int    `json:"local_port"`
	}
	if !decode(w, r, &input) {
		return
	}
	result, err := s.service.StartOperatorSSHTunnel(r.Context(), input.HostID, input.RemoteHost, input.RemotePort, input.LocalPort, actor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) stopSSHTunnel(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.StopSSHTunnel(r.Context(), r.PathValue("id"), actor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) saveHost(w http.ResponseWriter, r *http.Request) {
	var host domain.HostInput
	if !decodeLimit(w, r, &host, 3<<20) {
		return
	}
	result, err := s.service.SaveHost(r.Context(), host, actor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) getHost(w http.ResponseWriter, r *http.Request) {
	host, err := s.service.GetHost(r.Context(), r.PathValue("id"))
	respond(w, host, err)
}

func (s *Server) deleteHost(w http.ResponseWriter, r *http.Request) {
	err := s.service.DeleteHost(r.Context(), r.PathValue("id"), actor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) scanHostKey(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.ScanHostKey(r.Context(), r.PathValue("id"))
	respond(w, result, err)
}

func (s *Server) trustHostKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Fingerprint string `json:"fingerprint"`
	}
	if !decode(w, r, &input) {
		return
	}
	result, err := s.service.TrustHostKey(r.Context(), r.PathValue("id"), input.Fingerprint, actor(r))
	respond(w, result, err)
}

func (s *Server) probeHost(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.ProbeHost(r.Context(), r.PathValue("id"), actor(r))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, err)
		} else {
			writeErrorStatus(w, err, http.StatusBadGateway)
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listSFTPEntries(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.ListOperatorSFTPFiles(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	respond(w, result, err)
}

func (s *Server) downloadSFTPFile(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.OpenOperatorSFTPFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, err)
		return
	}
	defer result.Reader.Close()
	filename := path.Base(result.Entry.Path)
	contentType := mime.TypeByExtension(path.Ext(filename))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Content-Length", strconv.FormatInt(result.Entry.Size, 10))
	w.Header().Set("Cache-Control", "no-store")
	if _, err := io.Copy(w, result.Reader); err != nil {
		observability.FromContext(r.Context()).ErrorContext(r.Context(), "SFTP download failed", "component", "server", "host_id", r.PathValue("id"), "path", result.Entry.Path, "error", err)
	}
}

func (s *Server) uploadSFTPFile(w http.ResponseWriter, r *http.Request) {
	overwrite, _ := strconv.ParseBool(r.URL.Query().Get("overwrite"))
	source, err := sftpUploadReader(r.Body, r.URL.Query().Get("encoding"))
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadRequest)
		return
	}
	result, err := s.service.UploadOperatorSFTPFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"), source, overwrite)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeErrorStatus(w, err, status)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func sftpUploadReader(source io.Reader, encoding string) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "utf-8", "utf8":
		return source, nil
	case "utf-16le":
		return transform.NewReader(source, unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder()), nil
	case "utf-16be":
		return transform.NewReader(source, unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewEncoder()), nil
	case "gb18030", "gbk":
		return transform.NewReader(source, simplifiedchinese.GB18030.NewEncoder()), nil
	default:
		return nil, fmt.Errorf("unsupported text encoding %q", encoding)
	}
}

func (s *Server) createSFTPDirectory(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path string `json:"path"`
	}
	if !decode(w, r, &input) {
		return
	}
	result, err := s.service.CreateOperatorSFTPDirectory(r.Context(), r.PathValue("id"), input.Path)
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) renameSFTPEntry(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SourcePath      string `json:"source_path"`
		DestinationPath string `json:"destination_path"`
	}
	if !decode(w, r, &input) {
		return
	}
	result, err := s.service.RenameOperatorSFTPEntry(r.Context(), r.PathValue("id"), input.SourcePath, input.DestinationPath)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		writeErrorStatus(w, err, status)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) deleteSFTPEntry(w http.ResponseWriter, r *http.Request) {
	recursive, _ := strconv.ParseBool(r.URL.Query().Get("recursive"))
	result, err := s.service.RemoveOperatorSFTPEntry(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"), recursive)
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	var req domain.ExecRequest
	if !decode(w, r, &req) {
		return
	}
	result, err := s.service.Submit(r.Context(), req, actor(r))
	respond(w, result, err)
}

func (s *Server) startTask(w http.ResponseWriter, r *http.Request) {
	var req domain.ExecRequest
	if !decode(w, r, &req) {
		return
	}
	result, err := s.service.StartTask(r.Context(), req, actor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	task, result, taskErr, err := s.service.GetTask(r.PathValue("id"))
	respond(w, map[string]any{"task": task, "result": result, "error": taskErr}, err)
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	err := s.service.CancelTask(r.PathValue("id"), actor(r))
	respond(w, map[string]any{"cancelled": err == nil}, err)
}

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	result, err := s.service.ListApprovals(r.Context(), r.URL.Query().Get("status"), limit)
	respond(w, result, err)
}

func (s *Server) retryApprovalExplanation(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.RetryApprovalExplanation(r.Context(), r.PathValue("id"), actor(r))
	respond(w, result, err)
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &input) {
		return
	}
	result, err := s.service.ApproveAsync(r.Context(), r.PathValue("id"), input.Reason, actor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &input) {
		return
	}
	err := s.service.Reject(r.Context(), r.PathValue("id"), input.Reason, actor(r))
	respond(w, map[string]any{"rejected": err == nil}, err)
}

func (s *Server) searchRuns(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	result, err := s.service.SearchRuns(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("host_id"), limit)
	respond(w, result, err)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	includeRaw := r.URL.Query().Get("raw") == "1"
	result, err := s.service.GetRun(r.Context(), r.PathValue("id"), includeRaw)
	respond(w, result, err)
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	result, err := s.service.ListAudit(r.Context(), r.URL.Query().Get("run_id"), limit)
	respond(w, result, err)
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil || !s.agent.Available() {
		writeErrorStatus(w, agent.ErrUnavailable, http.StatusServiceUnavailable)
		return
	}
	sessionID, workspaceID, message, attachments, ok := s.decodeChatInput(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(message) == "" && len(attachments) == 0 {
		writeErrorStatus(w, fmt.Errorf("message or image is required"), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(sessionID) == "" {
		sessionID = ids.New("session")
	}
	if _, err := s.service.PrepareChatSession(r.Context(), sessionID, workspaceID, actor(r)); err != nil {
		writeErrorStatus(w, err, http.StatusBadRequest)
		return
	}
	streamAgentEvents(w, r, 10*time.Second, func(emit func(agent.Event)) {
		queryCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
		defer cancel()
		started := false
		publish := func(event agent.Event) {
			if event.Type == "session" {
				started = true
			}
			event = s.chatEvents.publish(sessionID, event)
			emit(event)
		}
		_, err := s.agent.QueryWithAttachments(queryCtx, sessionID, message, attachments, publish)
		if err != nil && !errors.Is(err, context.Canceled) {
			event := agent.Event{Type: "model_error", Error: err.Error(), SessionID: sessionID}
			if started {
				publish(event)
			} else {
				emit(event)
			}
		}
	})
}

func (s *Server) decodeChatInput(w http.ResponseWriter, r *http.Request) (string, string, string, []domain.ChatAttachment, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		writeErrorStatus(w, fmt.Errorf("invalid chat content type: %w", err), http.StatusBadRequest)
		return "", "", "", nil, false
	}
	if mediaType == "application/json" {
		var input struct {
			SessionID   string `json:"session_id"`
			WorkspaceID string `json:"workspace_id"`
			Message     string `json:"message"`
		}
		if !decode(w, r, &input) {
			return "", "", "", nil, false
		}
		return input.SessionID, input.WorkspaceID, input.Message, nil, true
	}
	if mediaType != "multipart/form-data" {
		writeErrorStatus(w, fmt.Errorf("chat content type must be application/json or multipart/form-data"), http.StatusUnsupportedMediaType)
		return "", "", "", nil, false
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErrorStatus(w, fmt.Errorf("invalid chat upload: %w", err), http.StatusBadRequest)
		return "", "", "", nil, false
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	settings, err := s.service.SystemSettings(r.Context())
	if err != nil {
		writeError(w, err)
		return "", "", "", nil, false
	}
	allowed := make(map[string]struct{}, len(settings.ChatImageAllowedTypes))
	for _, value := range settings.ChatImageAllowedTypes {
		allowed[value] = struct{}{}
	}
	files := r.MultipartForm.File["images"]
	attachments := make([]domain.ChatAttachment, 0, len(files))
	for index, header := range files {
		file, err := header.Open()
		if err != nil {
			writeErrorStatus(w, fmt.Errorf("open image %q: %w", header.Filename, err), http.StatusBadRequest)
			return "", "", "", nil, false
		}
		data, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if readErr != nil {
			writeErrorStatus(w, fmt.Errorf("read image %q: %w", header.Filename, readErr), http.StatusBadRequest)
			return "", "", "", nil, false
		}
		if closeErr != nil {
			writeErrorStatus(w, fmt.Errorf("close image %q: %w", header.Filename, closeErr), http.StatusBadRequest)
			return "", "", "", nil, false
		}
		if len(data) == 0 {
			writeErrorStatus(w, fmt.Errorf("image %q is empty", header.Filename), http.StatusBadRequest)
			return "", "", "", nil, false
		}
		mimeType := http.DetectContentType(data)
		if _, ok := allowed[mimeType]; !ok {
			writeErrorStatus(w, fmt.Errorf("image type %s is not enabled", mimeType), http.StatusBadRequest)
			return "", "", "", nil, false
		}
		name := path.Base(strings.ReplaceAll(header.Filename, "\\", "/"))
		if name == "." || name == "/" || name == "" {
			name = fmt.Sprintf("image-%d", index+1)
		}
		attachments = append(attachments, domain.ChatAttachment{Name: name, MIMEType: mimeType, SizeBytes: int64(len(data)), Data: data})
	}
	return r.FormValue("session_id"), r.FormValue("workspace_id"), r.FormValue("message"), attachments, true
}

// streamAgentEvents keeps the ResponseWriter owned by the HTTP goroutine while
// the Agent continues independently. This makes heartbeats and disconnects safe:
// a browser or proxy disappearing stops only the SSE writer, not the Agent loop.
func streamAgentEvents(w http.ResponseWriter, r *http.Request, heartbeatInterval time.Duration, run func(func(agent.Event))) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, fmt.Errorf("streaming is unavailable"))
		return
	}

	controller := http.NewResponseController(w)
	write := func(payload string) error {
		if _, err := fmt.Fprint(w, payload); err != nil {
			return err
		}
		return controller.Flush()
	}
	if err := write(": connected\n\n"); err != nil {
		return
	}

	events := make(chan agent.Event, 64)
	clientClosed := make(chan struct{})
	publish := func(event agent.Event) {
		select {
		case <-clientClosed:
			return
		default:
		}
		select {
		case events <- event:
		case <-clientClosed:
		}
	}
	go func() {
		defer close(events)
		defer func() {
			if recovered := recover(); recovered != nil {
				observability.FromContext(r.Context()).ErrorContext(r.Context(), "agent stream panic", "component", "agent", "error", recovered)
				publish(agent.Event{Type: "error", Error: "internal agent stream error"})
			}
		}()
		run(publish)
	}()

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	defer close(clientClosed)
	lastSessionID := ""
	logger := observability.FromContext(r.Context())
	for {
		select {
		case <-r.Context().Done():
			logger.DebugContext(r.Context(), "chat client disconnected; agent continues in background", "component", "agent", "session_id", lastSessionID, "error", r.Context().Err())
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			if event.SessionID != "" {
				lastSessionID = event.SessionID
			}
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if err := write(fmt.Sprintf("event: %s\ndata: %s\n\n", event.Type, data)); err != nil {
				logger.DebugContext(r.Context(), "chat client disconnected; agent continues in background", "component", "agent", "session_id", lastSessionID, "error", err)
				return
			}
		case <-heartbeat.C:
			if err := write(": heartbeat\n\n"); err != nil {
				logger.DebugContext(r.Context(), "chat heartbeat failed; agent continues in background", "component", "agent", "session_id", lastSessionID, "error", err)
				return
			}
		}
	}
}

func (s *Server) chatMessages(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.ListChatMessages(r.Context(), r.PathValue("id"), 0)
	respond(w, result, err)
}

func (s *Server) chatAttachment(w http.ResponseWriter, r *http.Request) {
	attachment, err := s.service.GetChatAttachment(r.Context(), r.PathValue("id"), r.PathValue("attachment_id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErrorStatus(w, err, http.StatusNotFound)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", attachment.MIMEType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": attachment.Name}))
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, attachment.Name, time.Time{}, bytes.NewReader(attachment.Data))
}

func (s *Server) chatState(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	session, err := s.service.GetChatSession(r.Context(), sessionID)
	if err != nil {
		writeError(w, err)
		return
	}
	messages, err := s.service.ListChatMessages(r.Context(), sessionID, 0)
	if err != nil {
		writeError(w, err)
		return
	}
	toolCalls, err := s.service.ListChatToolCalls(r.Context(), sessionID)
	if err != nil {
		writeError(w, err)
		return
	}
	var plan *domain.AgentPlan
	currentPlan, planErr := s.service.GetAgentPlan(r.Context(), sessionID)
	if planErr == nil {
		plan = &currentPlan
	} else if !errors.Is(planErr, store.ErrNotFound) {
		writeError(w, planErr)
		return
	}
	active := s.agent != nil && s.agent.IsSessionActive(sessionID)
	writeJSON(w, http.StatusOK, map[string]any{
		"active": active, "workspace_id": session.WorkspaceID,
		"context_tokens": session.ContextTokens, "context_window": session.ContextWindow,
		"messages": messages, "tool_calls": toolCalls, "plan": plan,
	})
}

func (s *Server) chatSessions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	result, err := s.service.ListChatSessions(r.Context(), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.agent != nil {
		for index := range result {
			result[index].Active = s.agent.IsSessionActive(result[index].ID)
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) cancelChatSession(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		writeErrorStatus(w, agent.ErrUnavailable, http.StatusServiceUnavailable)
		return
	}
	sessionID := strings.TrimSpace(r.PathValue("id"))
	if sessionID == "" {
		writeErrorStatus(w, fmt.Errorf("session id is required"), http.StatusBadRequest)
		return
	}
	cancelled := s.agent.CancelSession(sessionID)
	cancelledTools := 0
	rejectedApprovals := 0
	if s.service != nil {
		var err error
		cancelledTools, err = s.service.CancelSessionToolExecutions(r.Context(), sessionID)
		if err != nil {
			writeError(w, fmt.Errorf("cancel Agent session tools: %w", err))
			return
		}
		rejectedApprovals, err = s.service.RejectPendingApprovalsForSession(r.Context(), sessionID, "Agent run stopped by the operator", actor(r))
		if err != nil {
			writeError(w, fmt.Errorf("cancel Agent session approvals: %w", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": cancelled || cancelledTools > 0, "cancelled_tools": cancelledTools, "rejected_approvals": rejectedApprovals})
}

func (s *Server) setChatSessionWorkspace(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.PathValue("id"))
	if sessionID == "" {
		writeErrorStatus(w, fmt.Errorf("session id is required"), http.StatusBadRequest)
		return
	}
	if s.agent != nil && s.agent.IsSessionActive(sessionID) {
		writeErrorStatus(w, fmt.Errorf("cannot switch Workspace while this conversation's Agent run is active"), http.StatusConflict)
		return
	}
	var input struct {
		WorkspaceID string `json:"workspace_id"`
	}
	if !decode(w, r, &input) {
		return
	}
	session, err := s.service.SetChatSessionWorkspace(r.Context(), sessionID, input.WorkspaceID, actor(r))
	respond(w, session, err)
}

func (s *Server) deleteChatSession(w http.ResponseWriter, r *http.Request) {
	if s.agent != nil && s.agent.IsSessionActive(r.PathValue("id")) {
		writeErrorStatus(w, fmt.Errorf("cannot delete a conversation while its Agent run is active"), http.StatusConflict)
		return
	}
	if err := s.service.DeleteChatSession(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	s.chatEvents.delete(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	return decodeLimit(w, r, target, 2<<20)
}

func decodeLimit(w http.ResponseWriter, r *http.Request, target any, maxBytes int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeErrorStatus(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest)
		return false
	}
	return true
}

func respond(w http.ResponseWriter, value any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		status = http.StatusNotFound
	} else if errors.Is(err, service.ErrHostHasActiveTunnel) {
		status = http.StatusConflict
	} else if strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "expired") || strings.Contains(err.Error(), "mismatch") || strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "not a regular file") || strings.Contains(err.Error(), "can be deleted") {
		status = http.StatusBadRequest
	}
	writeErrorStatus(w, err, status)
}

func writeErrorStatus(w http.ResponseWriter, err error, status int) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func actor(r *http.Request) string {
	return "app"
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "http://localhost:5173" || origin == "http://127.0.0.1:5173" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				observability.FromContext(r.Context()).ErrorContext(r.Context(), "HTTP panic", "component", "http", "error", recovered, "path", r.URL.Path)
				writeErrorStatus(w, fmt.Errorf("internal server error"), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type logResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *logResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *logResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	written, err := w.ResponseWriter.Write(data)
	w.bytes += written
	return written, err
}

func (w *logResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *logResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

const slowReadRequestThreshold = 2 * time.Second

func requestLogMiddleware(next http.Handler, baseLogger *slog.Logger) http.Handler {
	if baseLogger == nil {
		baseLogger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := ids.New("request")
		logger := baseLogger.With("request_id", requestID)
		ctx := observability.WithLogger(r.Context(), logger)
		r = r.WithContext(ctx)
		w.Header().Set("X-Request-ID", requestID)
		recorder := &logResponseWriter{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		duration := time.Since(started)
		if status < http.StatusBadRequest && strings.HasPrefix(strings.ToLower(recorder.Header().Get("Content-Type")), "text/event-stream") {
			return
		}
		level, message, emit := requestLogDecision(r.Method, status, duration)
		if !emit {
			return
		}
		host := r.RemoteAddr
		if parsed, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			host = parsed
		}
		logger.With("component", "http").LogAttrs(ctx, level, message,
			slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Int("status", status),
			slog.Int64("duration_ms", duration.Milliseconds()), slog.Int("response_bytes", recorder.bytes), slog.String("remote_ip", host))
	})
}

func requestLogDecision(method string, status int, duration time.Duration) (slog.Level, string, bool) {
	if status >= http.StatusInternalServerError {
		return slog.LevelError, "HTTP request failed", true
	}
	if status >= http.StatusBadRequest {
		return slog.LevelWarn, "HTTP request rejected", true
	}
	readOnly := method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
	if readOnly {
		if duration >= slowReadRequestThreshold {
			return slog.LevelWarn, "slow HTTP read request completed", true
		}
		return 0, "", false
	}
	return slog.LevelInfo, "HTTP request completed", true
}

func spaHandler(root fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		clean := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if clean == "" || clean == "." {
			clean = "index.html"
		}
		if info, err := fs.Stat(root, clean); err == nil && !info.IsDir() {
			if clean == "index.html" {
				w.Header().Set("Cache-Control", "no-cache, max-age=0")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		index, err := root.Open("index.html")
		if err != nil {
			http.Error(w, "embedded web UI is unavailable", http.StatusNotFound)
			return
		}
		defer index.Close()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, max-age=0")
		_, _ = bufio.NewReader(index).WriteTo(w)
	})
}
