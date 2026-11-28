package mcp

// ToolDefinition describes an MCP tool exposed via tools/list.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// AllToolDefinitions returns the canonical, deterministically ordered definitions
// and JSON schemas for all 7 GoGraphCtx MCP tools.
func AllToolDefinitions() []ToolDefinition {
	return []ToolDefinition{
		{
			Name:        "find_symbol",
			Description: "Find code symbols (functions, classes, structs, interfaces, variables, type aliases) by name pattern, kind, file, or language.",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Symbol name or glob pattern (supports * and ?; plain text matches case-insensitive substring).",
					},
					"kind": map[string]any{
						"type":        "string",
						"description": "Optional symbol kind filter (Function, Class, Struct, Interface, Enum, Variable, TypeAlias, Property).",
					},
					"file": map[string]any{
						"type":        "string",
						"description": "Optional repo-relative file path or glob pattern.",
					},
					"language": map[string]any{
						"type":        "string",
						"description": "Optional canonical language filter (go, python, typescript, tsx, javascript, rust, java).",
					},
					"exported_only": map[string]any{
						"type":        "boolean",
						"description": "When true, return only exported/public symbols.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     200,
						"description": "Maximum symbols to return (default 50).",
					},
				},
			},
		},
		{
			Name:        "get_callers",
			Description: "Return direct callers of a target function or method with confidence tiers and exact call-site locations.",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"symbol"},
				"properties": map[string]any{
					"symbol": map[string]any{
						"type":        "string",
						"description": "Target symbol name, qualified_name (e.g. pkg::Type::Method), or 32-char UID hex.",
					},
					"file": map[string]any{
						"type":        "string",
						"description": "Optional repo-relative file path to disambiguate common short names.",
					},
					"include_likely": map[string]any{
						"type":        "boolean",
						"description": "Include Tier 7 LIKELY_CALLS candidates (default true).",
					},
					"limit": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     200,
						"description": "Maximum caller edges to return (default 50).",
					},
				},
			},
		},
		{
			Name:        "get_callees",
			Description: "Return functions and methods called by a source function with confidence tiers and source line sites.",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"symbol"},
				"properties": map[string]any{
					"symbol": map[string]any{
						"type":        "string",
						"description": "Caller symbol name, qualified_name, or 32-char UID hex.",
					},
					"file": map[string]any{
						"type":        "string",
						"description": "Optional repo-relative file path to disambiguate common short names.",
					},
					"include_likely": map[string]any{
						"type":        "boolean",
						"description": "Include Tier 7 LIKELY_CALLS candidates (default true).",
					},
					"limit": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     200,
						"description": "Maximum callee edges to return (default 50).",
					},
				},
			},
		},
		{
			Name:        "get_impact",
			Description: "Compute transitive reverse-dependency blast radius for a symbol up to max_depth hops, separated by certain vs likely edges.",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"symbol"},
				"properties": map[string]any{
					"symbol": map[string]any{
						"type":        "string",
						"description": "Target symbol name, qualified_name, or 32-char UID hex.",
					},
					"file": map[string]any{
						"type":        "string",
						"description": "Optional repo-relative file path to disambiguate common short names.",
					},
					"max_depth": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     10,
						"description": "Maximum transitive caller traversal depth (default 3).",
					},
					"limit": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     200,
						"description": "Maximum impacted nodes to return (default 50).",
					},
				},
			},
		},
		{
			Name:        "file_outline",
			Description: "Return the structured symbol outline (module, imports, classes/fields, functions/methods, variables, type aliases) of a source file.",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"file"},
				"properties": map[string]any{
					"file": map[string]any{
						"type":        "string",
						"description": "Repository-relative file path (e.g. internal/service/orders.go).",
					},
					"include_private": map[string]any{
						"type":        "boolean",
						"description": "Include unexported/private symbols (default true).",
					},
				},
			},
		},
		{
			Name:        "search_code",
			Description: "Multi-criteria search across indexed symbols filtering by name, qualified name, kind, language, file, export status, and cyclomatic complexity.",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"name_pattern": map[string]any{
						"type":        "string",
						"description": "Symbol name pattern (* / ? / substring).",
					},
					"qname_pattern": map[string]any{
						"type":        "string",
						"description": "Qualified name pattern (* / ? / substring).",
					},
					"kinds": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "List of node kinds (Function, Class, Struct, Interface, etc.).",
					},
					"languages": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "List of languages to include.",
					},
					"file_pattern": map[string]any{
						"type":        "string",
						"description": "File path pattern (* / ? / substring).",
					},
					"exported_only": map[string]any{
						"type":        "boolean",
						"description": "When true, match only exported symbols.",
					},
					"min_complexity": map[string]any{
						"type":        "integer",
						"minimum":     0,
						"description": "Minimum cyclomatic complexity filter.",
					},
					"max_complexity": map[string]any{
						"type":        "integer",
						"minimum":     0,
						"description": "Maximum cyclomatic complexity filter (0 means unbounded).",
					},
					"offset": map[string]any{
						"type":        "integer",
						"minimum":     0,
						"description": "Pagination offset.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     200,
						"description": "Maximum results to return (default 50).",
					},
				},
			},
		},
		{
			Name:        "repo_overview",
			Description: "Return aggregate repository statistics: file counts, symbol/edge breakdowns, language distribution, resolution coverage, and complexity distribution.",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"include_top_complex": map[string]any{
						"type":        "boolean",
						"description": "Include highest-complexity functions list (default true).",
					},
					"limit": map[string]any{
						"type":        "integer",
						"minimum":     1,
						"maximum":     50,
						"description": "Maximum top complex functions to include (default 10).",
					},
				},
			},
		},
	}
}
