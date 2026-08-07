package http

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// The request direction is NOT a shared vocabulary, and that is the whole
// problem this file exists for.
//
// ServerFrame is one type all three transports emit, so a new event frame is
// wired once. Requests are three separate structs: proto.ClientFrame (WS only),
// an anonymous struct inside Chat (SSE), and v1.TurnStartParams. json.Decode
// silently ignores unknown keys, so a turn-input field added to one of them
// reaches the other two as SILENCE — no parse error, no log line, the feature
// simply does nothing there. That is not hypothetical: image attachments POSTed
// to SSE disappeared exactly this way, and the fix was to declare the field a
// second and third time by hand.
//
// clientTurnInputFields names every ClientFrame field that carries TURN INPUT
// and the field each other request struct must declare for it. An empty target
// means "deliberately absent here" and requires a line in
// clientFieldAbsentReason.
var clientTurnInputFields = map[string]struct{ SSE, V1 string }{
	"Text":         {SSE: "Message", V1: "Input"},
	"Name":         {SSE: "Model", V1: "Model"},
	"Effort":       {SSE: "Thinking", V1: "Thinking"},
	"OutputSchema": {SSE: "OutputSchema", V1: "OutputSchema"},
	"Images":       {SSE: "Images", V1: "Images"},
	"Attachments":  {SSE: "Attachments", V1: ""},
}

// clientFieldAbsentReason justifies each empty target above.
//
// As with internal/cli/tui::frameTypesNotRendered, the reason must be a claim
// about STRUCTURE — something a reader can go and check — rather than about
// intent. A reason of the "we decided not to" kind cannot be falsified and so
// silently outlives whatever made it true.
var clientFieldAbsentReason = map[string]string{
	"Attachments/V1": "resolveAttachments needs a workRoot and a permission profile to " +
		"check the path against; internal/api/v1.Service is constructed with neither, so " +
		"declaring the field would accept @path references it could only ignore or resolve " +
		"unchecked. Nothing in sdk/ or docs/api advertises it.",
}

// clientControlOnlyFields names the ClientFrame fields that are arguments to WS
// CONTROL frames rather than turn input. SSE and v1 have no control channel —
// SSE is one POST per turn and v1's control surface is its own REST verbs — so
// there is nothing for these to be absent from.
var clientControlOnlyFields = map[string]string{
	"Type":          "the frame discriminator itself",
	"Mode":          "set_mode",
	"AutoThreshold": "set_mode",
	"ID":            "permission_response / session and skill verbs address a target",
	"Decision":      "permission_response",
	"ConfirmedHead": "restore_turn",
	"MCPServer":     "mcp_action",
	"MCPAction":     "mcp_action",
	"Seq":           "restore_session addresses a message index",
	"Source":        "install_skill",
	"FeaturesSet":   "features_set",
}

// TestEveryClientFrameTurnInputFieldReachesEveryTransport pins the request-side
// hop the way TestToStreamEventCarriesEveryServerFrameField pins the response
// side.
func TestEveryClientFrameTurnInputFieldReachesEveryTransport(t *testing.T) {
	clientFields := structFields(t, "../../proto/frame.go", "ClientFrame")
	require.NotEmpty(t, clientFields, "ClientFrame not found: the scan is broken, not the code")
	sseFields := sseRequestFields(t)
	require.NotEmpty(t, sseFields, "the SSE request struct was not found: the scan is broken")
	v1Fields := structFields(t, "../v1/types.go", "TurnStartParams")
	require.NotEmpty(t, v1Fields, "TurnStartParams not found: the scan is broken")

	// (1) Every ClientFrame field is classified exactly once, and neither table
	//     names one that no longer exists. A new wire field cannot reach WS
	//     without someone deciding, in writing, whether the other two need it.
	var unclassified []string
	for name := range clientFields {
		_, turn := clientTurnInputFields[name]
		_, ctrl := clientControlOnlyFields[name]
		require.Falsef(t, turn && ctrl, "%s is classified as both turn input and control-only", name)
		if !turn && !ctrl {
			unclassified = append(unclassified, name)
		}
	}
	sort.Strings(unclassified)
	require.Emptyf(t, unclassified,
		"proto.ClientFrame fields in neither clientTurnInputFields nor clientControlOnlyFields: %v\n"+
			"decide whether SSE and v1 must accept them — json.Decode will not tell you", unclassified)
	for name := range clientTurnInputFields {
		require.Truef(t, clientFields[name], "clientTurnInputFields names %q but ClientFrame has no such field", name)
	}
	for name := range clientControlOnlyFields {
		require.Truef(t, clientFields[name], "clientControlOnlyFields names %q but ClientFrame has no such field", name)
	}

	// (2) Turn-input fields are declared on the other two request structs, or
	//     declared absent with a structural reason.
	for name, targets := range clientTurnInputFields {
		checkTarget(t, name, "SSE", targets.SSE, sseFields,
			"internal/api/http/chat.go's request struct")
		checkTarget(t, name, "V1", targets.V1, v1Fields,
			"internal/api/v1.TurnStartParams")
	}

	// (3) Dead reasons: a justification for a field that IS now declared reads
	//     as a live constraint and would outlive the thing it described.
	for key := range clientFieldAbsentReason {
		var found bool
		for name, targets := range clientTurnInputFields {
			if key == name+"/SSE" && targets.SSE == "" {
				found = true
			}
			if key == name+"/V1" && targets.V1 == "" {
				found = true
			}
		}
		require.Truef(t, found,
			"clientFieldAbsentReason has an entry for %q, but that field is not declared absent", key)
	}
}

func checkTarget(t *testing.T, field, transport, target string, have map[string]bool, where string) {
	t.Helper()
	if target == "" {
		_, ok := clientFieldAbsentReason[field+"/"+transport]
		require.Truef(t, ok,
			"ClientFrame.%s is declared absent from %s with no reason — add one to "+
				"clientFieldAbsentReason saying what structurally prevents it", field, transport)
		// A declared absence that is no longer true is the failure this gate
		// exists to prevent, so it must not be possible to leave one here.
		// Found by probe: wiring the field into the target struct while the
		// table still said "absent" left the gate GREEN and the reason — a
		// paragraph asserting the field cannot work there — live and wrong.
		// That is exactly how the subagent_event exemption survived.
		require.Falsef(t, have[field],
			"ClientFrame.%s is declared absent from %s, but %s now declares it: "+
				"delete the clientFieldAbsentReason entry and give the field its target name",
			field, transport, where)
		return
	}
	require.Truef(t, have[target],
		"ClientFrame.%s should reach %s as %q, and %s declares no such field: "+
			"a request sent there is accepted and the value silently dropped",
		field, transport, target, where)
}

// structFields returns the field names of the named struct in a source file.
func structFields(t *testing.T, path, name string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	require.NoError(t, err)
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != name {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, fld := range st.Fields.List {
			for _, id := range fld.Names {
				out[id.Name] = true
			}
		}
		return false
	})
	return out
}

// sseRequestFields returns the field names of the anonymous request struct the
// Chat handler decodes into. It is anonymous and function-local on purpose (the
// shared vocabulary is ServerFrame only), which is exactly why it has to be
// found by shape rather than by name: the largest `var req struct{...}` in
// chat.go.
func sseRequestFields(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "chat.go", nil, 0)
	require.NoError(t, err)
	best := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "req" || vs.Type == nil {
			return true
		}
		st, ok := vs.Type.(*ast.StructType)
		if !ok {
			return true
		}
		got := map[string]bool{}
		for _, fld := range st.Fields.List {
			for _, id := range fld.Names {
				got[id.Name] = true
			}
		}
		if len(got) > len(best) {
			best = got
		}
		return true
	})
	return best
}
