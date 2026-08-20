package agent

import "github.com/cloudwego/eino/schema"

func init() {
	// Provider metadata can contain nested JSON objects and arrays. Message.Extra
	// itself is a concrete map, but its values are interfaces, so composite JSON
	// values must be registered before Eino's gob-based checkpoint is encoded.
	schema.RegisterName[map[string]any]("_opsnerva_map_string_any")
	schema.RegisterName[[]any]("_opsnerva_any_slice")
}
