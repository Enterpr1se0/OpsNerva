package httpapi

import "net/http"

func (s *Server) retrySSHTunnel(w http.ResponseWriter, r *http.Request) {
	if err := s.service.RetryOperatorSSHTunnel(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
