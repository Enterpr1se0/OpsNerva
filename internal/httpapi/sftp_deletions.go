package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/Enterpr1se0/opsnerva/internal/service"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

func (s *Server) deleteSFTPEntry(w http.ResponseWriter, r *http.Request) {
	recursive := false
	if value := r.URL.Query().Get("recursive"); value != "" {
		var err error
		recursive, err = strconv.ParseBool(value)
		if err != nil {
			writeErrorStatus(w, errors.New("recursive must be a boolean"), http.StatusBadRequest)
			return
		}
	}
	result, err := s.service.StartSFTPDeletion(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"), recursive)
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, service.ErrSFTPDeletionBusy):
			status = http.StatusConflict
		case errors.Is(err, service.ErrSFTPDeletionLimit):
			status = http.StatusTooManyRequests
		case errors.Is(err, store.ErrNotFound):
			status = http.StatusNotFound
		}
		writeErrorStatus(w, err, status)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) cancelSFTPDeletion(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.CancelSFTPDeletion(r.PathValue("id"))
	respond(w, result, err)
}
