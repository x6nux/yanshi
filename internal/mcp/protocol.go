package mcp

import (
	"fmt"
	"strings"
)

// preferredProtocolVersion is what this client asks for on initialize: the
// newest MCP protocol revision it can speak. The server's answer decides the
// actual version — see negotiateProtocolVersion.
const preferredProtocolVersion = "2025-06-18"

// supportedProtocolVersions is the set of MCP protocol revisions this client
// can speak. The span is deliberate: 2024-11-05 is the revision the first
// public MCP servers shipped against, 2025-03-26 added streamable HTTP and the
// MCP-Protocol-Version header, 2025-06-18 is the current one. A server is free
// to answer with any of them; the client follows.
var supportedProtocolVersions = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// negotiateProtocolVersion is the client half of the MCP initialize version
// handshake. The client asks for preferredProtocolVersion; the server answers
// with the revision it selected. If that answer is one this client speaks, it
// is adopted verbatim — that IS the "switch between 2024-11-05 and
// 2025-03-26" mechanism: which revision governs the session is the server's
// call, and the client follows. Anything else is a hard error.
//
// No silent downgrade: a version nobody negotiated means the method set and
// field semantics of every later call are interpreted against an agreement
// that was never made. Failing loudly names the version and the supported set
// so the operator can either pin the server or the client.
func negotiateProtocolVersion(serverOffered string) (string, error) {
	v := strings.TrimSpace(serverOffered)
	if v == "" {
		return "", fmt.Errorf("mcp: server answered initialize without a protocolVersion")
	}
	if !supportedProtocolVersions[v] {
		return "", fmt.Errorf(
			"mcp: server negotiated unsupported protocol version %q (client speaks %s)",
			v, strings.Join(supportedProtocolVersionList(), ", "))
	}
	return v, nil
}

// supportedProtocolVersionList returns the supported revisions newest-first,
// for error messages.
func supportedProtocolVersionList() []string {
	return []string{"2025-06-18", "2025-03-26", "2024-11-05"}
}

// answerProtocolVersion is the server half of the same handshake, used by this
// package's server-side kit consumers (the yanshi agent MCP server). When the
// client requests a revision this server speaks, it is echoed — that pinned
// revision then governs the session. When it requests something else (a newer
// or unknown revision), the server answers with its newest and lets the client
// decide: the MCP initialize contract says an unsupported answer is the
// client's signal to disconnect, which is the honest outcome for both sides.
func answerProtocolVersion(requested string) string {
	v := strings.TrimSpace(requested)
	if supportedProtocolVersions[v] {
		return v
	}
	return preferredProtocolVersion
}
