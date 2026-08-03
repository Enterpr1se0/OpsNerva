package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"eino-ops-agent/internal/domain"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

const mcpOAuthFlowTimeout = 10 * time.Minute

type mcpOAuthSession struct {
	ClientID     string           `json:"client_id"`
	ClientSecret string           `json:"client_secret,omitempty"`
	AuthURL      string           `json:"auth_url"`
	TokenURL     string           `json:"token_url"`
	AuthStyle    oauth2.AuthStyle `json:"auth_style"`
	RedirectURL  string           `json:"redirect_url"`
	Scopes       []string         `json:"scopes,omitempty"`
	AccessToken  string           `json:"access_token"`
	TokenType    string           `json:"token_type,omitempty"`
	RefreshToken string           `json:"refresh_token,omitempty"`
	Expiry       time.Time        `json:"expiry,omitempty"`
}

type mcpOAuthCallback struct {
	result *auth.AuthorizationResult
	err    error
}

type mcpOAuthFlow struct {
	serverID  string
	serverURL string
	actor     string
	cancel    context.CancelFunc

	mu               sync.Mutex
	authorizationURL string
	state            string
	err              error
	authReady        chan struct{}
	authReadyOnce    sync.Once
	callback         chan mcpOAuthCallback
	done             chan struct{}
}

type mcpSavingTokenSource struct {
	mu       sync.Mutex
	source   oauth2.TokenSource
	config   *oauth2.Config
	previous *oauth2.Token
	save     func(*oauth2.Config, *oauth2.Token) error
}

func (s *mcpSavingTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, err := s.source.Token()
	if err != nil {
		return nil, err
	}
	if !sameMCPOAuthToken(s.previous, token) {
		if err := s.save(s.config, token); err != nil {
			return nil, err
		}
		s.previous = cloneOAuthToken(token)
	}
	return token, nil
}

func (s *Service) BeginMCPOAuth(ctx context.Context, serverID, redirectURL, actorName string) (domain.MCPOAuthStart, error) {
	server, err := s.store.GetMCPServer(ctx, serverID)
	if err != nil {
		return domain.MCPOAuthStart{}, err
	}
	if server.Transport != domain.MCPTransportStreamableHTTP {
		return domain.MCPOAuthStart{}, fmt.Errorf("OAuth is supported only for Streamable HTTP MCP servers")
	}
	if !server.Enabled {
		return domain.MCPOAuthStart{}, fmt.Errorf("MCP server must be enabled before authorization")
	}
	if err := validateMCPOAuthRedirectURL(redirectURL); err != nil {
		return domain.MCPOAuthStart{}, err
	}

	flowCtx, cancel := context.WithTimeout(s.executionCtx, mcpOAuthFlowTimeout)
	flow := &mcpOAuthFlow{
		serverID: server.ID, serverURL: server.URL, actor: actorName, cancel: cancel,
		authReady: make(chan struct{}), callback: make(chan mcpOAuthCallback, 1), done: make(chan struct{}),
	}
	s.mcpOAuthMu.Lock()
	if previous := s.mcpOAuthByServer[server.ID]; previous != nil {
		previous.cancel()
	}
	s.mcpOAuthByServer[server.ID] = flow
	s.mcpOAuthMu.Unlock()
	s.setMCPRuntime(server.ID, &mcpRuntimeState{status: "connecting"})

	go s.runMCPOAuthFlow(flowCtx, flow, server, redirectURL)
	select {
	case <-flow.authReady:
		flow.mu.Lock()
		start := domain.MCPOAuthStart{AuthorizationURL: flow.authorizationURL}
		flow.mu.Unlock()
		return start, nil
	case <-flow.done:
		return domain.MCPOAuthStart{}, flowError(flow)
	case <-ctx.Done():
		cancel()
		return domain.MCPOAuthStart{}, ctx.Err()
	}
}

func (s *Service) CompleteMCPOAuth(ctx context.Context, state, code, issuer, authorizationError string) error {
	if state == "" {
		return fmt.Errorf("OAuth state is required")
	}
	s.mcpOAuthMu.Lock()
	flow := s.mcpOAuthFlows[state]
	s.mcpOAuthMu.Unlock()
	if flow == nil {
		return fmt.Errorf("OAuth flow is invalid or expired")
	}
	callback := mcpOAuthCallback{result: &auth.AuthorizationResult{Code: code, State: state, Iss: issuer}}
	if authorizationError != "" {
		callback.err = fmt.Errorf("OAuth authorization failed: %s", authorizationError)
	} else if code == "" {
		callback.err = fmt.Errorf("OAuth authorization code is required")
	}
	select {
	case flow.callback <- callback:
	case <-flow.done:
		return flowError(flow)
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-flow.done:
		return flowError(flow)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) ClearMCPOAuth(ctx context.Context, serverID, actorName string) (domain.MCPServer, error) {
	s.mcpSecretsMu.Lock()
	server, err := s.store.GetMCPServer(ctx, serverID)
	if err != nil {
		s.mcpSecretsMu.Unlock()
		return domain.MCPServer{}, err
	}
	secrets, err := s.decryptMCPSecrets(server.SecretsCipher)
	if err != nil {
		s.mcpSecretsMu.Unlock()
		return domain.MCPServer{}, err
	}
	secrets.OAuth = nil
	if err := s.persistMCPSecrets(ctx, server.ID, secrets); err != nil {
		s.mcpSecretsMu.Unlock()
		return domain.MCPServer{}, err
	}
	s.mcpSecretsMu.Unlock()
	s.cancelMCPOAuthFlow(server.ID)
	if server.Enabled {
		_ = s.ReconnectMCPServer(ctx, server.ID)
	} else {
		s.disconnectMCPServer(server.ID, "disabled")
	}
	s.audit(ctx, "", "mcp_oauth_cleared", actorName, map[string]any{"server_id": server.ID, "name": server.Name})
	return s.GetMCPServer(ctx, server.ID)
}

func (s *Service) runMCPOAuthFlow(ctx context.Context, flow *mcpOAuthFlow, server domain.MCPServer, redirectURL string) {
	defer flow.cancel()
	defer close(flow.done)
	defer s.removeMCPOAuthFlow(flow)

	secrets, err := s.decryptMCPSecrets(server.SecretsCipher)
	if err != nil {
		s.finishMCPOAuthFlow(flow, err)
		return
	}
	resourceClient := mcpResourceHTTPClient(secrets.Headers, true)
	oauthClient := &http.Client{Timeout: mcpCallTimeout, Transport: http.DefaultTransport}
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		RedirectURL: redirectURL,
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			RedirectURIs: []string{redirectURL}, TokenEndpointAuthMethod: "none",
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, ClientName: "OpsNerva",
		}},
		AuthorizationCodeFetcher: func(fetchCtx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			if err := s.publishMCPOAuthURL(flow, args.URL); err != nil {
				return nil, err
			}
			select {
			case callback := <-flow.callback:
				return callback.result, callback.err
			case <-fetchCtx.Done():
				return nil, fetchCtx.Err()
			}
		},
		RequestRefreshToken: true,
		Client:              oauthClient,
		NewTokenSource: func(tokenCtx context.Context, config *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
			if err := s.persistMCPOAuthSession(flow.serverID, config, token); err != nil {
				return nil, err
			}
			return s.savingMCPOAuthTokenSource(tokenCtx, flow.serverID, oauthClient, config, token), nil
		},
	})
	if err != nil {
		s.finishMCPOAuthFlow(flow, err)
		return
	}
	session, tools, err := s.connectMCPServerWithTransport(ctx, server, resourceClient, handler)
	if err != nil {
		if s.mcpOAuthFlowCurrent(flow) {
			s.setMCPRuntime(server.ID, &mcpRuntimeState{status: "error", lastError: err.Error()})
		}
		s.finishMCPOAuthFlow(flow, err)
		return
	}
	current, err := s.store.GetMCPServer(ctx, server.ID)
	if err != nil || !current.Enabled || current.URL != flow.serverURL || !s.mcpOAuthFlowCurrent(flow) {
		_ = session.Close()
		if err == nil {
			err = errors.New("MCP server changed during OAuth authorization")
		}
		s.finishMCPOAuthFlow(flow, err)
		return
	}
	now := time.Now().UTC()
	s.setMCPRuntime(server.ID, &mcpRuntimeState{status: "ready", connectedAt: &now, session: session, tools: tools})
	s.audit(context.Background(), "", "mcp_oauth_authorized", flow.actor, map[string]any{"server_id": server.ID, "name": server.Name})
	s.finishMCPOAuthFlow(flow, nil)
}

func (s *Service) publishMCPOAuthURL(flow *mcpOAuthFlow, authorizationURL string) error {
	parsed, err := url.Parse(authorizationURL)
	if err != nil {
		return fmt.Errorf("invalid OAuth authorization URL: %w", err)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		return fmt.Errorf("OAuth authorization URL has no state")
	}
	flow.mu.Lock()
	flow.authorizationURL = authorizationURL
	flow.state = state
	flow.mu.Unlock()
	s.mcpOAuthMu.Lock()
	if s.mcpOAuthByServer[flow.serverID] != flow {
		s.mcpOAuthMu.Unlock()
		return context.Canceled
	}
	s.mcpOAuthFlows[state] = flow
	s.mcpOAuthMu.Unlock()
	flow.authReadyOnce.Do(func() { close(flow.authReady) })
	return nil
}

func (s *Service) removeMCPOAuthFlow(flow *mcpOAuthFlow) {
	s.mcpOAuthMu.Lock()
	defer s.mcpOAuthMu.Unlock()
	if s.mcpOAuthByServer[flow.serverID] == flow {
		delete(s.mcpOAuthByServer, flow.serverID)
	}
	if flow.state != "" && s.mcpOAuthFlows[flow.state] == flow {
		delete(s.mcpOAuthFlows, flow.state)
	}
}

func (s *Service) cancelMCPOAuthFlow(serverID string) {
	s.mcpOAuthMu.Lock()
	flow := s.mcpOAuthByServer[serverID]
	if flow != nil {
		delete(s.mcpOAuthByServer, serverID)
		if flow.state != "" && s.mcpOAuthFlows[flow.state] == flow {
			delete(s.mcpOAuthFlows, flow.state)
		}
	}
	s.mcpOAuthMu.Unlock()
	if flow != nil {
		flow.cancel()
	}
}

func (s *Service) mcpOAuthFlowCurrent(flow *mcpOAuthFlow) bool {
	s.mcpOAuthMu.Lock()
	defer s.mcpOAuthMu.Unlock()
	return s.mcpOAuthByServer[flow.serverID] == flow
}

func (s *Service) cancelAllMCPOAuthFlows() {
	s.mcpOAuthMu.Lock()
	flows := make([]*mcpOAuthFlow, 0, len(s.mcpOAuthByServer))
	for _, flow := range s.mcpOAuthByServer {
		flows = append(flows, flow)
	}
	s.mcpOAuthFlows = make(map[string]*mcpOAuthFlow)
	s.mcpOAuthByServer = make(map[string]*mcpOAuthFlow)
	s.mcpOAuthMu.Unlock()
	for _, flow := range flows {
		flow.cancel()
	}
}

func (s *Service) finishMCPOAuthFlow(flow *mcpOAuthFlow, err error) {
	flow.mu.Lock()
	flow.err = err
	flow.mu.Unlock()
}

func flowError(flow *mcpOAuthFlow) error {
	flow.mu.Lock()
	defer flow.mu.Unlock()
	return flow.err
}

func (s *Service) restoredMCPOAuthHandler(serverID string, session *mcpOAuthSession, client *http.Client) (*auth.AuthorizationCodeHandler, error) {
	config, token := oauthConfigAndToken(session)
	credentials := &oauthex.ClientCredentials{ClientID: config.ClientID}
	if config.ClientSecret != "" {
		credentials.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: config.ClientSecret}
	}
	initial := s.savingMCPOAuthTokenSource(s.executionCtx, serverID, client, config, token)
	return auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		RedirectURL: config.RedirectURL, PreregisteredClient: credentials, RequestRefreshToken: true, Client: client,
		AuthorizationCodeFetcher: func(context.Context, *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			return nil, errors.New("MCP OAuth authorization must be renewed")
		},
		InitialTokenSource: initial,
		NewTokenSource: func(tokenCtx context.Context, updated *oauth2.Config, updatedToken *oauth2.Token) (oauth2.TokenSource, error) {
			if err := s.persistMCPOAuthSession(serverID, updated, updatedToken); err != nil {
				return nil, err
			}
			return s.savingMCPOAuthTokenSource(tokenCtx, serverID, client, updated, updatedToken), nil
		},
	})
}

func (s *Service) savingMCPOAuthTokenSource(ctx context.Context, serverID string, client *http.Client, config *oauth2.Config, token *oauth2.Token) oauth2.TokenSource {
	refreshCtx := context.WithValue(ctx, oauth2.HTTPClient, client)
	return &mcpSavingTokenSource{
		source: config.TokenSource(refreshCtx, token), config: config, previous: cloneOAuthToken(token),
		save: func(updatedConfig *oauth2.Config, updatedToken *oauth2.Token) error {
			return s.persistMCPOAuthSession(serverID, updatedConfig, updatedToken)
		},
	}
}

func (s *Service) persistMCPOAuthSession(serverID string, config *oauth2.Config, token *oauth2.Token) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.mcpSecretsMu.Lock()
	defer s.mcpSecretsMu.Unlock()
	server, err := s.store.GetMCPServer(ctx, serverID)
	if err != nil {
		return err
	}
	secrets, err := s.decryptMCPSecrets(server.SecretsCipher)
	if err != nil {
		return err
	}
	secrets.OAuth = &mcpOAuthSession{
		ClientID: config.ClientID, ClientSecret: config.ClientSecret, AuthURL: config.Endpoint.AuthURL,
		TokenURL: config.Endpoint.TokenURL, AuthStyle: config.Endpoint.AuthStyle, RedirectURL: config.RedirectURL,
		Scopes: slices.Clone(config.Scopes), AccessToken: token.AccessToken, TokenType: token.TokenType,
		RefreshToken: token.RefreshToken, Expiry: token.Expiry,
	}
	return s.persistMCPSecrets(ctx, serverID, secrets)
}

func (s *Service) persistMCPSecrets(ctx context.Context, serverID string, secrets mcpSecrets) error {
	payload, err := json.Marshal(secrets)
	if err != nil {
		return err
	}
	ciphertext, err := s.encryptor.Encrypt(payload)
	if err != nil {
		return err
	}
	return s.store.UpdateMCPServerSecrets(ctx, serverID, ciphertext)
}

func oauthConfigAndToken(session *mcpOAuthSession) (*oauth2.Config, *oauth2.Token) {
	return &oauth2.Config{
		ClientID: session.ClientID, ClientSecret: session.ClientSecret, RedirectURL: session.RedirectURL,
		Endpoint: oauth2.Endpoint{AuthURL: session.AuthURL, TokenURL: session.TokenURL, AuthStyle: session.AuthStyle},
		Scopes:   slices.Clone(session.Scopes),
	}, &oauth2.Token{AccessToken: session.AccessToken, TokenType: session.TokenType, RefreshToken: session.RefreshToken, Expiry: session.Expiry}
}

func validateMCPOAuthRedirectURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("invalid OAuth redirect URL")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	host := parsed.Hostname()
	if parsed.Scheme != "http" || (host != "localhost" && host != "127.0.0.1" && host != "[::1]" && host != "::1") {
		return fmt.Errorf("OAuth callback requires HTTPS or a loopback address")
	}
	return nil
}

func sameMCPOAuthToken(left, right *oauth2.Token) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.AccessToken == right.AccessToken && left.RefreshToken == right.RefreshToken && left.TokenType == right.TokenType && left.Expiry.Equal(right.Expiry)
}

func cloneOAuthToken(token *oauth2.Token) *oauth2.Token {
	if token == nil {
		return nil
	}
	clone := *token
	return &clone
}
