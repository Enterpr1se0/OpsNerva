package service

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/proxyx"
	"eino-ops-agent/internal/sshx"
	"eino-ops-agent/internal/store"
)

const maxConfigurationEntries = 2000

var configurationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

type configurationIdentity struct {
	ID   string
	Name string
}

func (s *Service) ExportConfiguration(ctx context.Context, includeSecrets bool, applicationVersion, actor string) (domain.ConfigurationPackage, error) {
	snapshot, err := s.store.ConfigurationSnapshot(ctx)
	if err != nil {
		return domain.ConfigurationPackage{}, err
	}
	result := domain.ConfigurationPackage{
		Schema: domain.ConfigurationSchema, SchemaVersion: domain.ConfigurationSchemaVersion,
		ApplicationVersion: applicationVersion, ExportedAt: time.Now().UTC(), SecretsIncluded: includeSecrets,
		Proxies: []domain.ConfigurationProxy{}, Hosts: []domain.ConfigurationHost{}, ModelProviders: []domain.ConfigurationModelProvider{},
	}
	for _, proxy := range snapshot.Proxies {
		item := domain.ConfigurationProxy{ID: proxy.ID, Name: proxy.Name, URL: proxy.URL, Username: proxy.Username, PasswordConfigured: proxy.PasswordCipher != ""}
		if includeSecrets && proxy.PasswordCipher != "" {
			plain, decryptErr := s.encryptor.Decrypt(proxy.PasswordCipher)
			if decryptErr != nil {
				return domain.ConfigurationPackage{}, fmt.Errorf("decrypt proxy %q password: %w", proxy.Name, decryptErr)
			}
			item.Password = string(plain)
		}
		result.Proxies = append(result.Proxies, item)
	}
	for _, host := range snapshot.Hosts {
		item := domain.ConfigurationHost{
			ID: host.ID, Name: host.Name, Address: host.Address, Port: host.Port, User: host.User, AgentEnabled: host.AgentEnabled,
			AuthType: host.AuthType, PrivateKeyConfigured: host.PrivateKeyCipher != "", KnownHostsFile: host.KnownHostsFile,
			ProxyJumpHostID: host.ProxyJumpHostID, ProxyID: host.ProxyID, PasswordConfigured: host.PasswordCipher != "",
			SudoMode: host.SudoMode, SudoPasswordConfigured: host.SudoCipher != "",
		}
		if includeSecrets {
			if host.PrivateKeyCipher != "" {
				plain, decryptErr := s.encryptor.Decrypt(host.PrivateKeyCipher)
				if decryptErr != nil {
					return domain.ConfigurationPackage{}, fmt.Errorf("decrypt host %q private key: %w", host.Name, decryptErr)
				}
				item.PrivateKey = string(plain)
			}
			if host.PasswordCipher != "" {
				plain, decryptErr := s.encryptor.Decrypt(host.PasswordCipher)
				if decryptErr != nil {
					return domain.ConfigurationPackage{}, fmt.Errorf("decrypt host %q password: %w", host.Name, decryptErr)
				}
				item.Password = string(plain)
			}
			if host.SudoCipher != "" {
				plain, decryptErr := s.encryptor.Decrypt(host.SudoCipher)
				if decryptErr != nil {
					return domain.ConfigurationPackage{}, fmt.Errorf("decrypt host %q sudo password: %w", host.Name, decryptErr)
				}
				item.SudoPassword = string(plain)
			}
		}
		result.Hosts = append(result.Hosts, item)
	}
	for _, provider := range snapshot.ModelProviders {
		item := domain.ConfigurationModelProvider{
			ID: provider.ID, Name: provider.Name, Kind: provider.Kind, BaseURL: provider.BaseURL, Model: provider.Model,
			ContextWindow: provider.ContextWindow, ReasoningEffort: provider.ReasoningEffort, APIKeyConfigured: provider.APIKeyCipher != "",
			ProxyID: provider.ProxyID, UserAgent: provider.UserAgent, Active: provider.Active,
		}
		if includeSecrets && provider.APIKeyCipher != "" {
			plain, decryptErr := s.encryptor.Decrypt(provider.APIKeyCipher)
			if decryptErr != nil {
				return domain.ConfigurationPackage{}, fmt.Errorf("decrypt model provider %q API key: %w", provider.Name, decryptErr)
			}
			item.APIKey = string(plain)
		}
		result.ModelProviders = append(result.ModelProviders, item)
	}
	s.audit(ctx, "", "configuration_exported", actor, map[string]any{
		"proxies": len(result.Proxies), "hosts": len(result.Hosts), "model_providers": len(result.ModelProviders), "secrets_included": includeSecrets,
	})
	return result, nil
}

func (s *Service) ImportConfiguration(ctx context.Context, input domain.ConfigurationPackage, encrypted bool, actor string) (domain.ConfigurationImportResult, error) {
	if input.Schema != domain.ConfigurationSchema || input.SchemaVersion != domain.ConfigurationSchemaVersion {
		return domain.ConfigurationImportResult{}, fmt.Errorf("unsupported configuration package schema or version")
	}
	if input.SecretsIncluded != encrypted {
		if input.SecretsIncluded {
			return domain.ConfigurationImportResult{}, fmt.Errorf("configuration packages containing credentials must be encrypted")
		}
		return domain.ConfigurationImportResult{}, fmt.Errorf("encrypted configuration package is missing its credential marker")
	}
	if len(input.Proxies) > maxConfigurationEntries || len(input.Hosts) > maxConfigurationEntries || len(input.ModelProviders) > maxConfigurationEntries {
		return domain.ConfigurationImportResult{}, fmt.Errorf("configuration package contains too many entries")
	}
	if !encrypted && configurationContainsSecrets(input) {
		return domain.ConfigurationImportResult{}, fmt.Errorf("unencrypted configuration package cannot contain credentials")
	}
	existing, err := s.store.ConfigurationSnapshot(ctx)
	if err != nil {
		return domain.ConfigurationImportResult{}, err
	}
	prepared := store.ConfigurationSnapshot{Proxies: []domain.Proxy{}, Hosts: []domain.Host{}, ModelProviders: []domain.ModelProvider{}}

	proxyIdentities := make([]configurationIdentity, 0, len(input.Proxies))
	existingProxyIdentities := make([]configurationIdentity, 0, len(existing.Proxies))
	existingProxies := make(map[string]domain.Proxy, len(existing.Proxies))
	for _, proxy := range existing.Proxies {
		existingProxies[proxy.ID] = proxy
		existingProxyIdentities = append(existingProxyIdentities, configurationIdentity{ID: proxy.ID, Name: proxy.Name})
	}
	for _, proxy := range input.Proxies {
		proxyIdentities = append(proxyIdentities, configurationIdentity{ID: strings.TrimSpace(proxy.ID), Name: strings.TrimSpace(proxy.Name)})
	}
	proxyIDs, err := resolveConfigurationIDs("proxy", proxyIdentities, existingProxyIdentities)
	if err != nil {
		return domain.ConfigurationImportResult{}, err
	}
	for index, source := range input.Proxies {
		identity := proxyIdentities[index]
		proxy, prepareErr := s.prepareImportedProxy(source, proxyIDs[identity.ID], existingProxies[proxyIDs[identity.ID]], encrypted)
		if prepareErr != nil {
			return domain.ConfigurationImportResult{}, fmt.Errorf("proxy %q: %w", identity.Name, prepareErr)
		}
		prepared.Proxies = append(prepared.Proxies, proxy)
	}

	hostIdentities := make([]configurationIdentity, 0, len(input.Hosts))
	existingHostIdentities := make([]configurationIdentity, 0, len(existing.Hosts))
	existingHosts := make(map[string]domain.Host, len(existing.Hosts))
	for _, host := range existing.Hosts {
		existingHosts[host.ID] = host
		existingHostIdentities = append(existingHostIdentities, configurationIdentity{ID: host.ID, Name: host.Name})
	}
	for _, host := range input.Hosts {
		hostIdentities = append(hostIdentities, configurationIdentity{ID: strings.TrimSpace(host.ID), Name: strings.TrimSpace(host.Name)})
	}
	hostIDs, err := resolveConfigurationIDs("host", hostIdentities, existingHostIdentities)
	if err != nil {
		return domain.ConfigurationImportResult{}, err
	}
	for index, source := range input.Hosts {
		identity := hostIdentities[index]
		targetID := hostIDs[identity.ID]
		source.ProxyID, err = remapConfigurationReference(strings.TrimSpace(source.ProxyID), proxyIDs, existingProxies, "proxy")
		if err != nil {
			return domain.ConfigurationImportResult{}, fmt.Errorf("host %q: %w", identity.Name, err)
		}
		source.ProxyJumpHostID, err = remapConfigurationReference(strings.TrimSpace(source.ProxyJumpHostID), hostIDs, existingHosts, "ProxyJump host")
		if err != nil {
			return domain.ConfigurationImportResult{}, fmt.Errorf("host %q: %w", identity.Name, err)
		}
		host, prepareErr := s.prepareImportedHost(source, targetID, existingHosts[targetID], encrypted)
		if prepareErr != nil {
			return domain.ConfigurationImportResult{}, fmt.Errorf("host %q: %w", identity.Name, prepareErr)
		}
		prepared.Hosts = append(prepared.Hosts, host)
	}
	if err := validateConfigurationHostGraph(existingHosts, prepared.Hosts, existingProxies, prepared.Proxies); err != nil {
		return domain.ConfigurationImportResult{}, err
	}

	providerIdentities := make([]configurationIdentity, 0, len(input.ModelProviders))
	existingProviderIdentities := make([]configurationIdentity, 0, len(existing.ModelProviders))
	existingProviders := make(map[string]domain.ModelProvider, len(existing.ModelProviders))
	for _, provider := range existing.ModelProviders {
		existingProviders[provider.ID] = provider
		existingProviderIdentities = append(existingProviderIdentities, configurationIdentity{ID: provider.ID, Name: provider.Name})
	}
	for _, provider := range input.ModelProviders {
		providerIdentities = append(providerIdentities, configurationIdentity{ID: strings.TrimSpace(provider.ID), Name: strings.TrimSpace(provider.Name)})
	}
	providerIDs, err := resolveConfigurationIDs("model provider", providerIdentities, existingProviderIdentities)
	if err != nil {
		return domain.ConfigurationImportResult{}, err
	}
	activeCount := 0
	for index, source := range input.ModelProviders {
		identity := providerIdentities[index]
		targetID := providerIDs[identity.ID]
		source.ProxyID, err = remapConfigurationReference(strings.TrimSpace(source.ProxyID), proxyIDs, existingProxies, "proxy")
		if err != nil {
			return domain.ConfigurationImportResult{}, fmt.Errorf("model provider %q: %w", identity.Name, err)
		}
		provider, prepareErr := s.prepareImportedModelProvider(source, targetID, existingProviders[targetID], encrypted)
		if prepareErr != nil {
			return domain.ConfigurationImportResult{}, fmt.Errorf("model provider %q: %w", identity.Name, prepareErr)
		}
		if provider.Active {
			activeCount++
		}
		prepared.ModelProviders = append(prepared.ModelProviders, provider)
	}
	if activeCount > 1 {
		return domain.ConfigurationImportResult{}, fmt.Errorf("configuration package contains more than one active model provider")
	}
	if err := s.store.ApplyConfiguration(ctx, prepared); err != nil {
		return domain.ConfigurationImportResult{}, err
	}
	result := domain.ConfigurationImportResult{
		Proxies: len(prepared.Proxies), Hosts: len(prepared.Hosts), ModelProviders: len(prepared.ModelProviders), SecretsImported: encrypted,
	}
	s.audit(ctx, "", "configuration_imported", actor, map[string]any{
		"proxies": result.Proxies, "hosts": result.Hosts, "model_providers": result.ModelProviders, "secrets_imported": result.SecretsImported,
	})
	return result, nil
}

func configurationContainsSecrets(input domain.ConfigurationPackage) bool {
	for _, proxy := range input.Proxies {
		if proxy.Password != "" {
			return true
		}
	}
	for _, host := range input.Hosts {
		if host.PrivateKey != "" || host.Password != "" || host.SudoPassword != "" {
			return true
		}
	}
	for _, provider := range input.ModelProviders {
		if provider.APIKey != "" {
			return true
		}
	}
	return false
}

func resolveConfigurationIDs(kind string, source, existing []configurationIdentity) (map[string]string, error) {
	existingByID := make(map[string]configurationIdentity, len(existing))
	existingByName := make(map[string]configurationIdentity, len(existing))
	for _, item := range existing {
		existingByID[item.ID] = item
		existingByName[item.Name] = item
	}
	result := make(map[string]string, len(source))
	seenNames := make(map[string]struct{}, len(source))
	seenTargets := make(map[string]struct{}, len(source))
	for _, item := range source {
		if !configurationIDPattern.MatchString(item.ID) {
			return nil, fmt.Errorf("%s id %q is invalid", kind, item.ID)
		}
		if item.Name == "" || len(item.Name) > 128 || containsCredentialControl(item.Name) {
			return nil, fmt.Errorf("%s name is invalid", kind)
		}
		if _, duplicate := result[item.ID]; duplicate {
			return nil, fmt.Errorf("duplicate %s id %q", kind, item.ID)
		}
		if _, duplicate := seenNames[item.Name]; duplicate {
			return nil, fmt.Errorf("duplicate %s name %q", kind, item.Name)
		}
		seenNames[item.Name] = struct{}{}
		targetID := item.ID
		if _, found := existingByID[item.ID]; !found {
			if match, nameFound := existingByName[item.Name]; nameFound {
				targetID = match.ID
			}
		}
		if match, found := existingByName[item.Name]; found && match.ID != targetID {
			return nil, fmt.Errorf("%s name %q conflicts with an existing entry", kind, item.Name)
		}
		if _, duplicate := seenTargets[targetID]; duplicate {
			return nil, fmt.Errorf("multiple %s entries resolve to id %q", kind, targetID)
		}
		seenTargets[targetID] = struct{}{}
		result[item.ID] = targetID
	}
	return result, nil
}

func remapConfigurationReference[T any](id string, imported map[string]string, existing map[string]T, kind string) (string, error) {
	if id == "" {
		return "", nil
	}
	if target, ok := imported[id]; ok {
		return target, nil
	}
	if _, ok := existing[id]; ok {
		return id, nil
	}
	return "", fmt.Errorf("references missing %s id %q", kind, id)
}

func validateConfigurationHostGraph(existingHosts map[string]domain.Host, importedHosts []domain.Host, existingProxies map[string]domain.Proxy, importedProxies []domain.Proxy) error {
	hosts := make(map[string]domain.Host, len(existingHosts)+len(importedHosts))
	importedHostIDs := make(map[string]struct{}, len(importedHosts))
	for id, host := range existingHosts {
		hosts[id] = host
	}
	for _, host := range importedHosts {
		hosts[host.ID] = host
		importedHostIDs[host.ID] = struct{}{}
	}
	proxies := make(map[string]domain.Proxy, len(existingProxies)+len(importedProxies))
	importedProxyIDs := make(map[string]struct{}, len(importedProxies))
	for id, proxy := range existingProxies {
		proxies[id] = proxy
	}
	for _, proxy := range importedProxies {
		proxies[proxy.ID] = proxy
		importedProxyIDs[proxy.ID] = struct{}{}
	}
	for _, host := range hosts {
		_, affected := importedHostIDs[host.ID]
		seen := map[string]struct{}{host.ID: {}}
		current := host
		depth := 0
		for {
			_, proxyAffected := importedProxyIDs[current.ProxyID]
			if current.ProxyID != "" && (affected || proxyAffected) {
				proxy, ok := proxies[current.ProxyID]
				if !ok {
					return fmt.Errorf("host %q references missing proxy id %q", host.Name, current.ProxyID)
				}
				if _, err := sshx.NormalizeProxyURL(proxy.URL); err != nil {
					return fmt.Errorf("host %q uses a proxy that is not compatible with SSH: %w", host.Name, err)
				}
			}
			if current.ProxyJumpHostID == "" {
				break
			}
			if depth >= maxProxyJumpDepth {
				if affected {
					return fmt.Errorf("host %q ProxyJump chain exceeds %d hosts", host.Name, maxProxyJumpDepth)
				}
				break
			}
			jump, ok := hosts[current.ProxyJumpHostID]
			if !ok {
				if affected {
					return fmt.Errorf("host %q references missing ProxyJump host id %q", host.Name, current.ProxyJumpHostID)
				}
				break
			}
			if _, imported := importedHostIDs[jump.ID]; imported {
				affected = true
			}
			if _, duplicate := seen[jump.ID]; duplicate {
				if affected {
					return fmt.Errorf("host %q ProxyJump chain contains a cycle", host.Name)
				}
				break
			}
			seen[jump.ID] = struct{}{}
			current = jump
			depth++
		}
	}
	return nil
}

func (s *Service) prepareImportedProxy(input domain.ConfigurationProxy, id string, existing domain.Proxy, secrets bool) (domain.Proxy, error) {
	name := strings.TrimSpace(input.Name)
	username := strings.TrimSpace(input.Username)
	proxyURL, err := proxyx.NormalizeURL(input.URL)
	if err != nil {
		return domain.Proxy{}, err
	}
	if proxyURL == "" || len(proxyURL) > 2048 {
		return domain.Proxy{}, fmt.Errorf("proxy URL is invalid")
	}
	if containsCredentialControl(username) || containsCredentialControl(input.Password) || len(username) > 255 || len(input.Password) > 255 {
		return domain.Proxy{}, fmt.Errorf("proxy credentials are invalid")
	}
	result := domain.Proxy{ID: id, Name: name, URL: proxyURL, Username: username}
	if secrets {
		if input.PasswordConfigured != (input.Password != "") {
			return domain.Proxy{}, fmt.Errorf("password marker does not match the encrypted credential")
		}
		if username == "" && input.PasswordConfigured {
			return domain.Proxy{}, fmt.Errorf("password requires a username")
		}
		if input.Password != "" {
			result.PasswordCipher, err = s.encryptor.Encrypt([]byte(input.Password))
			if err != nil {
				return domain.Proxy{}, err
			}
		}
	} else if existing.ID != "" && existing.Username == username {
		result.PasswordCipher = existing.PasswordCipher
	}
	return result, nil
}

func (s *Service) prepareImportedHost(input domain.ConfigurationHost, id string, existing domain.Host, secrets bool) (domain.Host, error) {
	name := strings.TrimSpace(input.Name)
	address := strings.TrimSpace(input.Address)
	user := strings.TrimSpace(input.User)
	authType := strings.TrimSpace(input.AuthType)
	sudoMode := strings.TrimSpace(input.SudoMode)
	if input.Port < 1 || input.Port > 65535 || address == "" || len(address) > 1024 || user == "" || len(user) > 255 || containsCredentialControl(address) || containsCredentialControl(user) {
		return domain.Host{}, fmt.Errorf("address, port, or user is invalid")
	}
	if authType == "" {
		authType = "agent"
	}
	if sudoMode == "" {
		sudoMode = "none"
	}
	if authType != "agent" && authType != "key" && authType != "password" {
		return domain.Host{}, fmt.Errorf("invalid SSH authentication type %q", authType)
	}
	if sudoMode != "none" && sudoMode != "nopasswd" && sudoMode != "password" {
		return domain.Host{}, fmt.Errorf("invalid sudo mode %q", sudoMode)
	}
	if input.ProxyJumpHostID == id && id != "" {
		return domain.Host{}, fmt.Errorf("a host cannot use itself as ProxyJump")
	}
	if len(input.KnownHostsFile) > 4096 || containsCredentialControl(input.KnownHostsFile) {
		return domain.Host{}, fmt.Errorf("known hosts file path is too long")
	}
	if containsCredentialControl(input.Password) || containsCredentialControl(input.SudoPassword) || len(input.Password) > 1024 || len(input.SudoPassword) > 1024 {
		return domain.Host{}, fmt.Errorf("SSH credentials are invalid")
	}
	var err error
	result := domain.Host{
		ID: id, Name: name, Address: address, Port: input.Port, User: user, AgentEnabled: input.AgentEnabled,
		AuthType: authType, KnownHostsFile: strings.TrimSpace(input.KnownHostsFile), ProxyJumpHostID: input.ProxyJumpHostID,
		ProxyID: input.ProxyID, SudoMode: sudoMode,
	}
	if secrets {
		if input.PrivateKeyConfigured != (input.PrivateKey != "") || input.PasswordConfigured != (input.Password != "") || input.SudoPasswordConfigured != (input.SudoPassword != "") {
			return domain.Host{}, fmt.Errorf("credential marker does not match the encrypted credentials")
		}
		if authType != "key" && input.PrivateKey != "" || authType != "password" && input.Password != "" || sudoMode != "password" && input.SudoPassword != "" {
			return domain.Host{}, fmt.Errorf("credentials do not match the selected authentication modes")
		}
		if input.PrivateKey != "" {
			if err := sshx.ValidatePrivateKey([]byte(input.PrivateKey)); err != nil {
				return domain.Host{}, fmt.Errorf("invalid SSH private key: %w", err)
			}
			result.PrivateKeyCipher, err = s.encryptor.Encrypt([]byte(input.PrivateKey))
			if err != nil {
				return domain.Host{}, err
			}
		}
		if input.Password != "" {
			result.PasswordCipher, err = s.encryptor.Encrypt([]byte(input.Password))
			if err != nil {
				return domain.Host{}, err
			}
		}
		if input.SudoPassword != "" {
			result.SudoCipher, err = s.encryptor.Encrypt([]byte(input.SudoPassword))
			if err != nil {
				return domain.Host{}, err
			}
		}
	} else if existing.ID != "" {
		if authType == "key" {
			result.PrivateKeyCipher = existing.PrivateKeyCipher
		}
		if authType == "password" {
			result.PasswordCipher = existing.PasswordCipher
		}
		if sudoMode == "password" {
			result.SudoCipher = existing.SudoCipher
		}
	}
	if authType == "key" && result.PrivateKeyCipher == "" || authType == "password" && result.PasswordCipher == "" {
		result.AgentEnabled = false
	}
	return result, nil
}

func (s *Service) prepareImportedModelProvider(input domain.ConfigurationModelProvider, id string, existing domain.ModelProvider, secrets bool) (domain.ModelProvider, error) {
	name := strings.TrimSpace(input.Name)
	kind := strings.TrimSpace(input.Kind)
	model := strings.TrimSpace(input.Model)
	if model == "" || len(model) > 512 {
		return domain.ModelProvider{}, fmt.Errorf("model is invalid")
	}
	switch kind {
	case "openai", "deepseek", "anthropic", "openai_compatible", "ollama":
	default:
		return domain.ModelProvider{}, fmt.Errorf("invalid provider kind %q", kind)
	}
	baseURL, err := normalizeProviderBaseURL(input.BaseURL, kind)
	if err != nil {
		return domain.ModelProvider{}, err
	}
	if input.ContextWindow != 0 && (input.ContextWindow < domain.MinModelContextWindow || input.ContextWindow > domain.MaxModelContextWindow) {
		return domain.ModelProvider{}, fmt.Errorf("context_window must be between %d and %d", domain.MinModelContextWindow, domain.MaxModelContextWindow)
	}
	reasoningEffort, err := normalizeReasoningEffort(input.ReasoningEffort)
	if err != nil {
		return domain.ModelProvider{}, err
	}
	userAgent, err := validateProviderUserAgent(input.UserAgent)
	if err != nil {
		return domain.ModelProvider{}, err
	}
	if len(input.APIKey) > 64<<10 || containsCredentialControl(input.APIKey) {
		return domain.ModelProvider{}, fmt.Errorf("API key is invalid")
	}
	result := domain.ModelProvider{
		ID: id, Name: name, Kind: kind, BaseURL: baseURL, Model: model, ContextWindow: input.ContextWindow,
		ReasoningEffort: reasoningEffort, ProxyID: input.ProxyID, UserAgent: userAgent, Active: input.Active,
	}
	if secrets {
		if input.APIKeyConfigured != (input.APIKey != "") {
			return domain.ModelProvider{}, fmt.Errorf("API key marker does not match the encrypted credential")
		}
		if input.APIKey != "" {
			result.APIKeyCipher, err = s.encryptor.Encrypt([]byte(strings.TrimSpace(input.APIKey)))
			if err != nil {
				return domain.ModelProvider{}, err
			}
		}
	} else if existing.ID != "" {
		result.APIKeyCipher = existing.APIKeyCipher
	}
	if providerKindRequiresAPIKey(kind) && result.APIKeyCipher == "" {
		result.Active = false
	}
	return result, nil
}
