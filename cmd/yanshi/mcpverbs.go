package main

// isMcpManagementVerb reports whether `yanshi mcp` was invoked as the
// management verb rather than the stdio MCP server.
//
// The two share a name on purpose: an operator typing `yanshi mcp` means "the
// MCP thing", and forcing them to remember which spelling is the server and
// which is the client is the kind of API the TUI's /mcp does not impose. The
// server form is recognised by its own flags (it takes -config/-fake-model and
// no positional verb), and the grammar is unambiguous: a leading positional
// word can only be a management verb.
func isMcpManagementVerb(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "list", "enable", "disable":
		return true
	}
	return false
}
