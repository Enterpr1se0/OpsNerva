package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func (s *Server) createWorkspaceDirectory(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path string `json:"path"`
	}
	if !decode(w, r, &input) {
		return
	}
	if err := s.service.CreateAdminWorkspaceDirectory(r.Context(), r.PathValue("id"), input.Path); err != nil {
		writeErrorStatus(w, err, http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
