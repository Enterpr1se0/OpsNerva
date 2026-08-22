package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"eino-ops-agent/internal/domain"
)

const maxConfigurationPackageBytes = 32 << 20

func (s *Server) exportConfiguration(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.ExportConfiguration(r.Context(), s.options.Version, actor(r))
	if err != nil {
		writeError(w, err)
		return
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		writeError(w, err)
		return
	}
	payload = append(payload, '\n')
	filename := "opsnerva-configuration-" + time.Now().UTC().Format("20060102-150405") + ".json"
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func (s *Server) importConfiguration(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigurationPackageBytes+(1<<20))
	if err := r.ParseMultipartForm(maxConfigurationPackageBytes); err != nil {
		writeErrorStatus(w, fmt.Errorf("invalid configuration upload: %w", err), http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErrorStatus(w, fmt.Errorf("configuration package is required"), http.StatusBadRequest)
		return
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, maxConfigurationPackageBytes+1))
	if err != nil {
		writeError(w, err)
		return
	}
	if len(payload) == 0 || len(payload) > maxConfigurationPackageBytes {
		writeErrorStatus(w, fmt.Errorf("configuration package must be between 1 byte and 32 MiB"), http.StatusBadRequest)
		return
	}
	configuration, err := decodeConfigurationPackage(payload)
	if err != nil {
		writeErrorStatus(w, err, http.StatusBadRequest)
		return
	}
	result, err := s.service.ImportConfiguration(r.Context(), configuration, actor(r))
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint") || strings.Contains(strings.ToLower(err.Error()), "conflict") {
			status = http.StatusConflict
		}
		writeErrorStatus(w, err, status)
		return
	}
	if s.agent != nil {
		if reloadErr := s.agent.Reload(r.Context()); reloadErr != nil {
			slog.WarnContext(r.Context(), "agent reload after configuration import failed", "component", "server", "error", reloadErr)
		} else {
			result.RuntimeReloaded = true
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func decodeConfigurationPackage(payload []byte) (domain.ConfigurationPackage, error) {
	var result domain.ConfigurationPackage
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return domain.ConfigurationPackage{}, fmt.Errorf("invalid configuration package: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return domain.ConfigurationPackage{}, fmt.Errorf("invalid configuration package: trailing content")
	}
	return result, nil
}
