// Package agentassets holds lockmq's agent texts (skill, instructions,
// Cursor rule, example config) and their wiring through lockkit's
// agentwire.
package agentassets

import (
	_ "embed"

	"github.com/DavidGodefroid/lockkit/agentwire"

	"github.com/DavidGodefroid/lockmq/internal/product"
)

var (
	//go:embed skill.md
	Skill []byte
	//go:embed agents.md
	Instructions []byte
	//go:embed cursor-rule.mdc
	CursorFrontMatter []byte
	//go:embed config.toml
	ProjectConfig []byte
)

// Wiring wires the agents to lockmq. Claude Code may call, without a
// prompt, only what never shows message content without the console's
// approval.
var Wiring = agentwire.Wiring{Product: product.P, Assets: agentwire.Assets{
	MCPCommand:        []string{"lockmq", "mcp"},
	Skill:             Skill,
	Instructions:      Instructions,
	CursorFrontMatter: CursorFrontMatter,
	ProjectConfig:     ProjectConfig,
	ClaudeAllow: []string{
		"mcp__lockmq__lockmq_status", "mcp__lockmq__lockmq_overview", "mcp__lockmq__lockmq_queues", "mcp__lockmq__lockmq_queue",
		"mcp__lockmq__lockmq_exchanges", "mcp__lockmq__lockmq_bindings", "mcp__lockmq__lockmq_policies",
		"mcp__lockmq__lockmq_consumers", "mcp__lockmq__lockmq_connections", "mcp__lockmq__lockmq_channels",
		"mcp__lockmq__lockmq_stream", "mcp__lockmq__lockmq_stream_offset", "mcp__lockmq__lockmq_pii_list",
		"Bash(lockmq status:*)", "Bash(lockmq doctor:*)",
	},
	ClaudeNote: "Message reads (lockmq_read, lockmq_peek) are approved in the lockmq console, not in Claude Code.",
}}
