package cli

import (
	"context"
	nethttp "net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/proto"
)

// streamEventFieldMap declares, for every proto.ServerFrame field, the
// cli.StreamEvent field that toStreamEvent must copy it into. Most entries are
// same-name; the renames (Type→Kind, Status→ToolStatus, Names/Servers→Items)
// are spelled out here rather than inferred, because an inferred mapping would
// quietly treat a rename as "not carried".
//
// This table exists because toStreamEvent is the ONE hop both transports share
// (wsBackend.readLoop and sseBackend.flush both call it) and it had zero
// field-level coverage: dropping a single assignment left the whole suite green
// while the field arrived at the TUI as its zero value. force_prompt was the
// live instance — StreamEvent.ForcePrompt stuck at false meant the TUI's
// mandatory flag was false and YOLO auto-answered "allow" for requests the
// server had explicitly refused to auto-resolve.
var streamEventFieldMap = map[string]string{
	"Type":             "Kind",
	"Text":             "Text",
	"ToolName":         "ToolName",
	"ToolArgs":         "ToolArgs",
	"Status":           "ToolStatus",
	"Overwrite":        "Overwrite",
	"Names":            "Items",
	"Servers":          "Items",
	"Model":            "Model",
	"Thinking":         "Thinking",
	"PermMode":         "PermMode",
	"AutoThreshold":    "AutoThreshold",
	"TokensIn":         "TokensIn",
	"TokensOut":        "TokensOut",
	"CachedTokens":     "CachedTokens",
	"ReasoningTokens":  "ReasoningTokens",
	"Turns":            "Turns",
	"ContextWindow":    "ContextWindow",
	"ID":               "ID",
	"Reason":           "Reason",
	"ApprovalRequired": "ApprovalRequired",
	"ForcePrompt":      "ForcePrompt",
	"Compacted":        "Compacted",
	"TokensBefore":     "TokensBefore",
	"TokensAfter":      "TokensAfter",
	"Messages":         "Messages",
	"RetryAttempt":     "RetryAttempt",
	"RetryMax":         "RetryMax",
	"RetryDelayMs":     "RetryDelayMs",
	"Sessions":         "Sessions",
	"SessionID":        "SessionID",
	"MemoryPath":       "MemoryPath",
	"LogPath":          "LogPath",
	"SideDepth":        "SideDepth",
	"Skills":           "Skills",
	"Skill":            "Skill",
	"Action":           "Action",
	"StructuredResult": "StructuredResult",
	"Seams":            "Seams",
	"CommitShort":      "CommitShort",
	"Head":             "Head",
	"Permissions":      "Permissions",
	"Jobs":             "Jobs",
	"MCPServers":       "MCPServers",
	"Task":             "Task",
	"TaskID":           "TaskID",
	"Checklist":        "Checklist",
	"CostUSD":          "CostUSD",
	"CostKnown":        "CostKnown",
	"Features":         "Features",
}

// streamEventNotCarried lists the proto.ServerFrame fields toStreamEvent
// deliberately does NOT surface to the TUI, with the reason. Adding a field
// here is a decision, not a default: the parity test fails on any ServerFrame
// field absent from both tables, so a newly added wire field cannot reach the
// client half-wired without someone writing a line in one of them.
var streamEventNotCarried = map[string]string{
	"AgentID":     "subagent_event: the TUI renders subagent lifecycle from the tool blocks, not these ids",
	"AgentRole":   "subagent_event: see AgentID",
	"Event":       "subagent_event: see AgentID",
	"AgentStatus": "subagent_event: see AgentID",
}

// TestToStreamEventCarriesEveryServerFrameField is the field-level parity gate
// for the shared ServerFrame → StreamEvent hop. For every mapped field it sets
// a non-zero sentinel on an otherwise empty frame and asserts the declared
// StreamEvent field comes out non-zero; for every field declared not-carried it
// asserts the same-named StreamEvent field (when one exists) stays zero, so a
// wrong "not carried" claim also fails. Dead entries in either table fail too.
func TestToStreamEventCarriesEveryServerFrameField(t *testing.T) {
	frameType := reflect.TypeOf(proto.ServerFrame{})
	eventType := reflect.TypeOf(StreamEvent{})

	// (1) Every ServerFrame field is classified exactly once, and neither table
	//     names a field that no longer exists.
	declared := map[string]bool{}
	for name := range streamEventFieldMap {
		declared[name] = true
	}
	for name := range streamEventNotCarried {
		_, dup := streamEventFieldMap[name]
		assert.Falsef(t, dup, "%s is declared both carried and not-carried", name)
		declared[name] = true
	}
	for i := 0; i < frameType.NumField(); i++ {
		name := frameType.Field(i).Name
		assert.Truef(t, declared[name],
			"proto.ServerFrame.%s is in neither streamEventFieldMap nor streamEventNotCarried: "+
				"decide whether toStreamEvent must carry it to the TUI", name)
		delete(declared, name)
	}
	for name := range declared {
		t.Errorf("%s is declared in a streamEvent table but proto.ServerFrame has no such field", name)
	}

	// (2) Carried fields actually propagate.
	for frameField, eventField := range streamEventFieldMap {
		ft, ok := frameType.FieldByName(frameField)
		if !ok {
			continue // already reported above
		}
		if _, ok := eventType.FieldByName(eventField); !ok {
			t.Errorf("cli.StreamEvent has no field %s (declared as the target of ServerFrame.%s)",
				eventField, frameField)
			continue
		}
		frame := reflect.New(frameType).Elem()
		frame.FieldByName(frameField).Set(sentinelValue(t, ft.Type))
		got := reflect.ValueOf(toStreamEvent(frame.Interface().(proto.ServerFrame))).FieldByName(eventField)
		assert.Truef(t, isPopulated(got),
			"toStreamEvent dropped ServerFrame.%s: StreamEvent.%s is still zero", frameField, eventField)
	}

	// (3) Not-carried fields really are not carried (a same-named StreamEvent
	//     field left zero proves the table entry, not just the author's memory).
	for frameField, reason := range streamEventNotCarried {
		ft, ok := frameType.FieldByName(frameField)
		if !ok {
			continue
		}
		if _, ok := eventType.FieldByName(frameField); !ok {
			continue // StreamEvent has no such field at all: nothing to observe
		}
		frame := reflect.New(frameType).Elem()
		frame.FieldByName(frameField).Set(sentinelValue(t, ft.Type))
		got := reflect.ValueOf(toStreamEvent(frame.Interface().(proto.ServerFrame))).FieldByName(frameField)
		assert.Falsef(t, isPopulated(got),
			"ServerFrame.%s is declared not-carried (%s) but toStreamEvent does carry it", frameField, reason)
	}
}

// sentinelValue builds a non-zero value of type t for the parity test. Byte
// slices get valid JSON bytes (json.RawMessage is one), other slices get a
// one-element slice of zero values (length alone makes them observably
// non-zero), pointers get a fresh zero element.
func sentinelValue(t *testing.T, typ reflect.Type) reflect.Value {
	t.Helper()
	switch typ.Kind() {
	case reflect.Bool:
		return reflect.ValueOf(true).Convert(typ)
	case reflect.String:
		return reflect.ValueOf("sentinel").Convert(typ)
	case reflect.Int, reflect.Int64:
		return reflect.ValueOf(7).Convert(typ)
	case reflect.Float64:
		return reflect.ValueOf(1.5).Convert(typ)
	case reflect.Slice:
		if typ.Elem().Kind() == reflect.Uint8 {
			return reflect.ValueOf([]byte(`1`)).Convert(typ)
		}
		return reflect.MakeSlice(typ, 1, 1)
	case reflect.Ptr:
		return reflect.New(typ.Elem())
	}
	t.Fatalf("sentinelValue: unsupported kind %s (%s) — extend this helper", typ.Kind(), typ)
	return reflect.Value{}
}

// isPopulated reports whether v holds the sentinel rather than the zero value.
// Slices are judged by length (their elements are intentionally zero).
func isPopulated(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Slice, reflect.Map:
		return v.Len() > 0
	case reflect.Ptr, reflect.Interface:
		return !v.IsNil()
	default:
		return !v.IsZero()
	}
}

// TestWSBackend_ForcePromptReachesStreamEvent closes the last untested hop of
// the force_prompt chain: server frame → JSON on the socket → wsBackend.readLoop
// → toStreamEvent → StreamEvent. The two halves either side of it were already
// pinned (ws_perm_test.go stops at the ServerFrame, perm_mode_test.go starts at
// the StreamEvent), and deleting toStreamEvent's ForcePrompt assignment left the
// entire suite green — the TUI then read mandatory=false and YOLO answered
// "allow" for a request the server refused to auto-resolve.
//
// It goes over a real socket rather than calling toStreamEvent directly so the
// json tag is covered too: a mistyped `force_prompt` tag drops the field just as
// silently as a missing assignment.
func TestWSBackend_ForcePromptReachesStreamEvent(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *nethttp.Request) bool { return true }}
	ts := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		sc, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer sc.Close()
		if _, _, err := sc.ReadMessage(); err != nil {
			return
		}
		_ = sc.WriteJSON(proto.NewPermissionRequest("p1", "task_cancel", "{}",
			"tool requires explicit approval", false, true))
		_ = sc.WriteJSON(proto.NewDone())
	}))
	defer ts.Close()

	b, err := newWSBackend(context.Background(), "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	require.NoError(t, err)
	defer b.Close()

	ch, err := b.Send(context.Background(), "hi")
	require.NoError(t, err)

	var got *StreamEvent
	for ev := range ch {
		if ev.Kind == "permission_request" {
			e := ev
			got = &e
		}
	}
	require.NotNil(t, got, "no permission_request event reached the client")
	assert.Equal(t, "p1", got.ID)
	assert.True(t, got.ForcePrompt,
		"force_prompt lost between the wire and StreamEvent: the TUI would treat this "+
			"request as ordinary and auto-answer it on the next YOLO switch")
	assert.False(t, got.ApprovalRequired, "approval_required must not be fabricated")
}
