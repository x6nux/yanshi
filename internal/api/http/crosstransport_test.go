package http

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/api/v1"
	"github.com/x6nux/yanshi/internal/appserver"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// HTTP and JSON-RPC are two front doors onto ONE *v1.Service, and nothing
// compared what they return. The service is shared, but the mapping from
// service result to wire response is written twice — once per transport — and
// that seam is where ThreadSnapshot.Items was forwarded identically wrong by
// both, and where a field added to one handler simply would not appear in the
// other.
//
// These tests drive the same operation down both paths against the same
// service and compare the decoded bodies. The comparison is on decoded JSON,
// not bytes: JSON-RPC wraps its payload in a result envelope, and demanding
// byte equality would only assert that the two envelopes differ.

// crossFixture builds one service and both transports over it.
func crossFixture(t *testing.T) (*httptest.Server, func(method, params string) map[string]any) {
	ts, raw := crossFixtureRaw(t)
	return ts, func(method, params string) map[string]any {
		msg := raw(method, params)
		if e, ok := msg["error"]; ok {
			t.Fatalf("rpc %s returned an error: %v", method, e)
		}
		result, _ := msg["result"].(map[string]any)
		return result
	}
}

// crossFixtureRaw is the same fixture without the success assertion, so error
// paths can be compared too.
func crossFixtureRaw(t *testing.T) (*httptest.Server, func(method, params string) map[string]any) {
	t.Helper()
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(Config{})
	s.AgentV1(svc)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	rpc := func(method, params string) map[string]any {
		t.Helper()
		in := strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + "}\n")
		var out bytes.Buffer
		srv := appserver.New(svc, appserver.NewMemoryConfig())
		if err := srv.Serve(t.Context(), in, &out); err != nil {
			t.Fatalf("appserver.Serve: %v", err)
		}
		var msg map[string]any
		line, _, _ := strings.Cut(strings.TrimSpace(out.String()), "\n")
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("rpc response %q: %v", line, err)
		}
		return msg
	}
	return ts, rpc
}

func postJSON(t *testing.T, ts *httptest.Server, path, body string) map[string]any {
	t.Helper()
	resp, err := ts.Client().Post(ts.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("POST %s body %q: %v", path, data, err)
	}
	return out
}

// volatile fields differ per call by design (ids, timestamps) and would make
// any comparison fail for the wrong reason.
var volatileKeys = map[string]bool{
	"id": true, "createdAt": true, "updatedAt": true, "startedAt": true,
	"completedAt": true, "threadId": true, "turnId": true,
}

// sameShape reports the paths where two decoded responses disagree, ignoring
// volatile values but NOT their presence: a transport that drops "id"
// entirely is a real divergence, one that returns a different id is not.
func sameShape(t *testing.T, path string, a, b any, diffs *[]string) {
	t.Helper()
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*diffs = append(*diffs, path+": object vs "+describe(b))
			return
		}
		for k, v := range av {
			sub, exists := bv[k]
			if !exists {
				*diffs = append(*diffs, path+"."+k+": present over HTTP, absent over JSON-RPC")
				continue
			}
			if volatileKeys[k] {
				continue
			}
			sameShape(t, path+"."+k, v, sub, diffs)
		}
		for k := range bv {
			if _, exists := av[k]; !exists {
				*diffs = append(*diffs, path+"."+k+": present over JSON-RPC, absent over HTTP")
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			*diffs = append(*diffs, path+": array vs "+describe(b))
			return
		}
		if len(av) != len(bv) {
			*diffs = append(*diffs, path+": lengths differ")
			return
		}
		for i := range av {
			sameShape(t, path+"[]", av[i], bv[i], diffs)
		}
	default:
		if a != b {
			*diffs = append(*diffs, path+": HTTP has "+describe(a)+", JSON-RPC has "+describe(b))
		}
	}
}

func describe(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// TestThreadStartAgreesAcrossTransports compares thread/start over both doors.
//
// ledger: D1/APS1#3 与 HTTP 行为一致
func TestThreadStartAgreesAcrossTransports(t *testing.T) {
	ts, rpc := crossFixture(t)
	body := `{"title":"cross","model":"fake","thinking":"low"}`

	httpResp := postJSON(t, ts, "/api/v1/thread/start", body)
	rpcResp := rpc("thread/start", body)

	var diffs []string
	sameShape(t, "thread/start", httpResp, rpcResp, &diffs)
	if len(diffs) > 0 {
		t.Errorf("the two transports describe thread/start differently:\n  %s",
			strings.Join(diffs, "\n  "))
	}
}

// TestThreadResumeAgreesAcrossTransports is the operation that was wrong in
// both transports at once: each forwarded snapshot.Items, and each forwarded
// a field the service never set.
//
// ledger: D1/APS1#3 与 HTTP 行为一致
func TestThreadResumeAgreesAcrossTransports(t *testing.T) {
	ts, rpc := crossFixture(t)
	// Both transports must resume a thread the OTHER one created, which is the
	// stronger claim: it proves they share the service rather than each
	// keeping its own state.
	started := postJSON(t, ts, "/api/v1/thread/start", `{"title":"cross"}`)
	thread, _ := started["thread"].(map[string]any)
	id, _ := thread["id"].(string)
	if id == "" {
		t.Fatalf("thread/start returned no id: %v", started)
	}
	body := `{"threadId":"` + id + `"}`

	httpResp := postJSON(t, ts, "/api/v1/thread/resume", body)
	rpcResp := rpc("thread/resume", body)

	var diffs []string
	sameShape(t, "thread/resume", httpResp, rpcResp, &diffs)
	if len(diffs) > 0 {
		t.Errorf("the two transports describe thread/resume differently:\n  %s",
			strings.Join(diffs, "\n  "))
	}
}

// TestInterruptAgreesAcrossTransports covers the idempotent path: interrupting
// a thread with no active turn must look the same either way.
//
// ledger: D1/APS1#3 与 HTTP 行为一致
func TestInterruptAgreesAcrossTransports(t *testing.T) {
	ts, rpc := crossFixture(t)
	started := postJSON(t, ts, "/api/v1/thread/start", `{}`)
	thread, _ := started["thread"].(map[string]any)
	id, _ := thread["id"].(string)
	body := `{"threadId":"` + id + `"}`

	httpResp := postJSON(t, ts, "/api/v1/thread/interrupt", body)
	rpcResp := rpc("thread/interrupt", body)

	var diffs []string
	sameShape(t, "thread/interrupt", httpResp, rpcResp, &diffs)
	if len(diffs) > 0 {
		t.Errorf("the two transports describe thread/interrupt differently:\n  %s",
			strings.Join(diffs, "\n  "))
	}
}

// postStatus returns the HTTP status for a request, for the error-path
// comparison.
func postStatus(t *testing.T, ts *httptest.Server, path, body string) int {
	t.Helper()
	resp, err := ts.Client().Post(ts.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}

// TestErrorsAgreeAcrossTransports compares the FAILURE direction.
//
// The two transports map v1 service errors independently — HTTP to status
// codes, JSON-RPC to standard codes — and the happy-path comparison above says
// nothing about that half. A client written against one door and moved to the
// other must not find an operation that fails on one and succeeds on the
// other; the codes differ by design, the verdict must not.
//
// Equivalence class, not equality: 404 and -32602 are the same answer said in
// two vocabularies. What this pins is that both say NO, and that neither
// silently succeeds.
func TestErrorsAgreeAcrossTransports(t *testing.T) {
	ts, rpc := crossFixtureRaw(t)

	cases := []struct {
		name       string
		httpPath   string
		rpcMethod  string
		body       string
		wantHTTPOK bool
	}{
		{"unknown thread on resume", "/api/v1/thread/resume", "thread/resume",
			`{"threadId":"no-such-thread"}`, false},
		{"unknown thread on turn start", "/api/v1/turn/start", "turn/start",
			`{"threadId":"no-such-thread","input":"hi"}`, false},
		{"missing threadId on resume", "/api/v1/thread/resume", "thread/resume",
			`{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := postStatus(t, ts, tc.httpPath, tc.body)
			httpOK := status < 400

			msg := rpc(tc.rpcMethod, tc.body)
			_, hasErr := msg["error"]
			rpcOK := !hasErr

			if httpOK != rpcOK {
				t.Errorf("the transports disagree on whether this fails: "+
					"HTTP %d (ok=%v), JSON-RPC ok=%v (%v)", status, httpOK, rpcOK, msg)
			}
			if httpOK != tc.wantHTTPOK {
				t.Errorf("HTTP returned %d, want a failure", status)
			}
		})
	}
}
