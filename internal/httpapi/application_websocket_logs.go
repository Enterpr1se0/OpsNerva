package httpapi

import "github.com/Enterpr1se0/opsnerva/internal/observability"

type applicationLogResponse struct {
	Entries      []observability.LogEntry `json:"entries"`
	Components   []string                 `json:"components,omitempty"`
	MinimumLevel string                   `json:"minimum_level,omitempty"`
	File         string                   `json:"file,omitempty"`
}

func applicationLogFilter(filter applicationWebSocketLogFilter) observability.LogFilter {
	return observability.LogFilter{Level: filter.Level, Component: filter.Component, Query: filter.Query, Limit: filter.Limit}
}

func applicationLogSnapshot(entries []observability.LogEntry) applicationLogResponse {
	return applicationLogResponse{
		Entries: entries, Components: observability.Components(), MinimumLevel: observability.MinimumLevel(), File: observability.File(),
	}
}
