package agenttool

import (
	"encoding/json"

	einojsonschema "github.com/eino-contrib/jsonschema"
)

type actionSchemaVariant struct {
	action   string
	required []string
	allowed  []string
	extend   func(*einojsonschema.Schema)
}

func applyActionSchema(root *einojsonschema.Schema, variants ...actionSchemaVariant) {
	root.OneOf = make([]*einojsonschema.Schema, 0, len(variants))
	for _, variant := range variants {
		properties := einojsonschema.NewProperties()
		properties.Set("action", &einojsonschema.Schema{Const: variant.action})
		branch := &einojsonschema.Schema{
			Type:       "object",
			Properties: properties,
			Required:   append([]string(nil), variant.required...),
		}
		allowed := make(map[string]struct{}, len(variant.allowed))
		for _, field := range variant.allowed {
			allowed[field] = struct{}{}
		}
		forbidden := make([]*einojsonschema.Schema, 0)
		for pair := root.Properties.Oldest(); pair != nil; pair = pair.Next() {
			if _, ok := allowed[pair.Key]; ok {
				continue
			}
			forbidden = append(forbidden, &einojsonschema.Schema{Required: []string{pair.Key}})
		}
		if len(forbidden) != 0 {
			branch.Not = &einojsonschema.Schema{AnyOf: forbidden}
		}
		if variant.extend != nil {
			variant.extend(branch)
		}
		root.OneOf = append(root.OneOf, branch)
	}
}

func constProperty(name, value string) *einojsonschema.Schema {
	properties := einojsonschema.NewProperties()
	properties.Set(name, &einojsonschema.Schema{Const: value})
	return &einojsonschema.Schema{Properties: properties}
}

func forbidFieldCombinations(root *einojsonschema.Schema, combinations ...[]string) {
	forbidden := make([]*einojsonschema.Schema, 0, len(combinations))
	for _, fields := range combinations {
		forbidden = append(forbidden, &einojsonschema.Schema{Required: append([]string(nil), fields...)})
	}
	if len(forbidden) != 0 {
		root.Not = &einojsonschema.Schema{AnyOf: forbidden}
	}
}

func requirePositiveInteger(root *einojsonschema.Schema, trigger, field string) {
	positive := einojsonschema.NewProperties()
	positive.Set(field, &einojsonschema.Schema{Minimum: json.Number("1")})
	root.AllOf = append(root.AllOf, &einojsonschema.Schema{
		If:   &einojsonschema.Schema{Required: []string{trigger}},
		Then: &einojsonschema.Schema{Properties: positive},
	})
}

func forbidTrueWithFields(root *einojsonschema.Schema, booleanField string, fields ...string) {
	for _, field := range fields {
		properties := einojsonschema.NewProperties()
		properties.Set(booleanField, &einojsonschema.Schema{Const: true})
		root.AllOf = append(root.AllOf, &einojsonschema.Schema{Not: &einojsonschema.Schema{
			Properties: properties,
			Required:   []string{booleanField, field},
		}})
	}
}

func forbidTruePair(root *einojsonschema.Schema, first, second string) {
	properties := einojsonschema.NewProperties()
	properties.Set(first, &einojsonschema.Schema{Const: true})
	properties.Set(second, &einojsonschema.Schema{Const: true})
	root.AllOf = append(root.AllOf, &einojsonschema.Schema{Not: &einojsonschema.Schema{
		Properties: properties,
		Required:   []string{first, second},
	}})
}

func (ExecInput) JSONSchemaExtend(root *einojsonschema.Schema) {
	root.DependentRequired = map[string][]string{"output_view": {"max_output_bytes"}}
	requirePositiveInteger(root, "output_view", "max_output_bytes")
}

func (ScriptInput) JSONSchemaExtend(root *einojsonschema.Schema) {
	root.DependentRequired = map[string][]string{"output_view": {"max_output_bytes"}}
	requirePositiveInteger(root, "output_view", "max_output_bytes")
}

func (SSHTunnelInput) JSONSchemaExtend(root *einojsonschema.Schema) {
	applyActionSchema(root,
		actionSchemaVariant{
			action:   "start",
			required: []string{"action", "host_id", "reason"},
			allowed:  []string{"action", "host_id", "direction", "local_host", "local_port", "remote_host", "remote_port", "reason"},
			extend: func(branch *einojsonschema.Schema) {
				local := constProperty("direction", "local")
				local.Type = "object"
				local.Required = []string{"remote_port"}
				local.Properties.Set("remote_port", &einojsonschema.Schema{Minimum: json.Number("1")})
				reverse := constProperty("direction", "reverse")
				reverse.Type = "object"
				reverse.Required = []string{"direction", "local_port"}
				reverse.Properties.Set("local_port", &einojsonschema.Schema{Minimum: json.Number("1")})
				branch.OneOf = []*einojsonschema.Schema{local, reverse}
			},
		},
		actionSchemaVariant{action: "list", required: []string{"action"}, allowed: []string{"action"}},
		actionSchemaVariant{action: "stop", required: []string{"action", "tunnel_id"}, allowed: []string{"action", "tunnel_id"}},
	)
}

func (TaskInput) JSONSchemaExtend(root *einojsonschema.Schema) {
	root.DependentRequired = map[string][]string{
		"block_until": {"wait_seconds"},
		"output_view": {"max_output_bytes"},
	}
	requirePositiveInteger(root, "block_until", "wait_seconds")
	requirePositiveInteger(root, "output_view", "max_output_bytes")
	applyActionSchema(root,
		actionSchemaVariant{
			action:   "status",
			required: []string{"action", "task_id"},
			allowed:  []string{"action", "task_id", "wait_seconds", "block_until", "after_stdout_bytes", "after_stderr_bytes", "max_output_bytes", "output_view"},
		},
		actionSchemaVariant{action: "cancel", required: []string{"action", "task_id"}, allowed: []string{"action", "task_id"}},
	)
}

func (SSHShellInput) JSONSchemaExtend(root *einojsonschema.Schema) {
	applyActionSchema(root,
		actionSchemaVariant{action: "start", required: []string{"action", "host_id", "reason"}, allowed: []string{"action", "host_id", "cwd", "elevated", "reason"}},
		actionSchemaVariant{action: "input", required: []string{"action", "shell_id", "input"}, allowed: []string{"action", "shell_id", "input", "submit", "wait_seconds", "max_output_bytes", "reason"}},
		actionSchemaVariant{action: "output", required: []string{"action", "shell_id"}, allowed: []string{"action", "shell_id", "after_sequence", "wait_seconds", "max_output_bytes", "reason"}},
		actionSchemaVariant{action: "list", required: []string{"action"}, allowed: []string{"action", "reason"}},
		actionSchemaVariant{action: "interrupt", required: []string{"action", "shell_id"}, allowed: []string{"action", "shell_id", "reason"}},
		actionSchemaVariant{action: "close", required: []string{"action", "shell_id"}, allowed: []string{"action", "shell_id", "reason"}},
	)
}

func (WorkspaceShellInput) JSONSchemaExtend(root *einojsonschema.Schema) {
	applyActionSchema(root,
		actionSchemaVariant{action: "run", required: []string{"action", "script", "reason"}, allowed: []string{"action", "script", "cwd", "env", "timeout_seconds", "reason"}},
		actionSchemaVariant{action: "start", required: []string{"action", "reason"}, allowed: []string{"action", "cwd", "env", "reason"}},
		actionSchemaVariant{action: "input", required: []string{"action", "shell_id", "input"}, allowed: []string{"action", "shell_id", "input", "submit", "wait_seconds", "max_output_bytes", "reason"}},
		actionSchemaVariant{action: "output", required: []string{"action", "shell_id"}, allowed: []string{"action", "shell_id", "after_sequence", "wait_seconds", "max_output_bytes", "reason"}},
		actionSchemaVariant{action: "list", required: []string{"action"}, allowed: []string{"action", "reason"}},
		actionSchemaVariant{action: "interrupt", required: []string{"action", "shell_id"}, allowed: []string{"action", "shell_id", "reason"}},
		actionSchemaVariant{action: "close", required: []string{"action", "shell_id"}, allowed: []string{"action", "shell_id", "reason"}},
	)
}

func (FileReadInput) JSONSchemaExtend(root *einojsonschema.Schema) {
	root.DependentRequired = map[string][]string{
		"pattern":       {"match_mode"},
		"match_mode":    {"pattern"},
		"context_lines": {"pattern"},
	}
	forbidFieldCombinations(root,
		[]string{"pattern", "max_bytes"},
		[]string{"pattern", "offset_bytes"},
		[]string{"pattern", "tail_lines"},
		[]string{"offset_bytes", "tail_lines"},
	)
	forbidTrueWithFields(root, "metadata_only", "pattern", "max_bytes", "offset_bytes", "tail_lines")
	forbidTrueWithFields(root, "full_content", "pattern", "max_bytes", "offset_bytes", "tail_lines")
	forbidTruePair(root, "metadata_only", "full_content")
}

func (WorkspaceReadInput) JSONSchemaExtend(root *einojsonschema.Schema) {
	root.DependentRequired = map[string][]string{
		"pattern":       {"match_mode"},
		"match_mode":    {"pattern"},
		"context_lines": {"pattern"},
	}
	forbidFieldCombinations(root,
		[]string{"pattern", "max_bytes"},
		[]string{"pattern", "offset_bytes"},
		[]string{"pattern", "tail_lines"},
		[]string{"offset_bytes", "tail_lines"},
	)
	forbidTrueWithFields(root, "full_content", "pattern", "max_bytes", "offset_bytes", "tail_lines")
}

func (HistorySearchInput) JSONSchemaExtend(root *einojsonschema.Schema) {
	root.DependentRequired = map[string][]string{
		"match_mode":         {"query"},
		"query_scope":        {"query"},
		"after_stdout_bytes": {"run_id"},
		"after_stderr_bytes": {"run_id"},
		"max_output_bytes":   {"run_id"},
		"output_view":        {"run_id"},
	}
	forbidFieldCombinations(root,
		[]string{"run_id", "host_id"},
		[]string{"run_id", "tool_name"},
		[]string{"run_id", "status"},
		[]string{"run_id", "started_after"},
		[]string{"run_id", "started_before"},
		[]string{"run_id", "cursor"},
		[]string{"run_id", "query", "after_stdout_bytes"},
		[]string{"run_id", "query", "after_stderr_bytes"},
		[]string{"run_id", "query", "output_view"},
	)
	root.AllOf = append(root.AllOf, &einojsonschema.Schema{
		If:   &einojsonschema.Schema{Required: []string{"run_id", "limit"}},
		Then: &einojsonschema.Schema{Required: []string{"query"}},
	})
}

func (WebSearchInput) JSONSchemaExtend(root *einojsonschema.Schema) {
	root.DependentRequired = map[string][]string{"chunks_per_source": {"search_depth"}}
	forbidFieldCombinations(root,
		[]string{"time_range", "start_date"},
		[]string{"time_range", "end_date"},
	)
	root.AllOf = append(root.AllOf, &einojsonschema.Schema{
		If:   &einojsonschema.Schema{Required: []string{"chunks_per_source"}},
		Then: constProperty("search_depth", "advanced"),
	})
}

func (WebExtractInput) JSONSchemaExtend(root *einojsonschema.Schema) {
	root.DependentRequired = map[string][]string{"chunks_per_source": {"query"}}
}
