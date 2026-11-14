package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// A request as pi-ai builds it when it does not know it is talking to the
// subscription: everything the subscription accepts, plus what it refuses.
const responsesRequestWithUnsupportedParts = `{
  "model": "gpt-x",
  "input": [
    {"role": "developer", "content": "be brief"},
    {"role": "user", "content": [{"type": "input_text", "text": "hello"}]},
    {"type": "reasoning", "id": "rs_1", "summary": [], "encrypted_content": "opaque-reasoning"},
    {"type": "configuration_update", "reasoning": {"effort": "high"}},
    {"type": "function_call", "call_id": "call_1", "name": "run", "arguments": "{}"},
    {"type": "function_call_output", "call_id": "call_1", "output": "done"},
    {"type": "configuration_update", "reasoning": {"effort": "xhigh"}}
  ],
  "stream": true,
  "prompt_cache_key": "session-1",
  "prompt_cache_retention": "24h",
  "prompt_cache_options": {"ttl": "30m"},
  "store": false,
  "max_output_tokens": 32000,
  "temperature": 0.2,
  "reasoning": {"effort": "medium", "summary": "auto"},
  "include": ["reasoning.encrypted_content"],
  "tools": [{"type": "function", "name": "run", "parameters": {"type": "object"}}]
}`

func postResponses(f *fixture, body io.Reader, header http.Header) *http.Response {
	f.t.Helper()
	if header == nil {
		header = http.Header{}
	}
	if header.Get("Content-Type") == "" {
		header.Set("Content-Type", "application/json")
	}
	return f.do(http.MethodPost, "/v1/responses", body, withAssertion(header))
}

func decodeObject(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("the upstream got a body that is not a JSON object: %v\n%s", err, b)
	}
	return m
}

func inputTypes(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var items []struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("input is not an array of items: %v\n%s", err, raw)
	}
	var types []string
	for _, item := range items {
		if item.Type == "" {
			types = append(types, "message:"+item.Role)
			continue
		}
		types = append(types, item.Type)
	}
	return types
}

func TestFieldsTheSubscriptionRefusesAreDroppedFromAResponsesRequest(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	resp := postResponses(f, strings.NewReader(responsesRequestWithUnsupportedParts), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := decodeObject(t, f.last().body)
	for _, name := range []string{"prompt_cache_retention", "prompt_cache_options", "max_output_tokens", "temperature"} {
		if v, ok := got[name]; ok {
			t.Errorf("%s reached the upstream: %s", name, v)
		}
	}
}

func TestConfigurationUpdateItemsAreDroppedFromTheInput(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	postResponses(f, strings.NewReader(responsesRequestWithUnsupportedParts), nil)
	got := decodeObject(t, f.last().body)
	want := []string{"message:developer", "message:user", "reasoning", "function_call", "function_call_output"}
	if types := inputTypes(t, got["input"]); !slices.Equal(types, want) {
		t.Errorf("input items = %v, want %v", types, want)
	}
}

func TestWhatTheSubscriptionAcceptsReachesTheUpstreamAsTheCallerWroteIt(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	postResponses(f, strings.NewReader(responsesRequestWithUnsupportedParts), nil)
	got := decodeObject(t, f.last().body)
	sent := decodeObject(t, []byte(responsesRequestWithUnsupportedParts))
	for _, name := range []string{"model", "stream", "prompt_cache_key", "store", "include", "tools"} {
		if !jsonEqual(t, got[name], sent[name]) {
			t.Errorf("%s = %s, want %s", name, got[name], sent[name])
		}
	}
	// The items kept are byte for byte what the caller sent: the reasoning
	// item's encrypted content and the function call reach the upstream.
	for _, item := range []string{
		`{"type": "reasoning", "id": "rs_1", "summary": [], "encrypted_content": "opaque-reasoning"}`,
		`{"type": "function_call", "call_id": "call_1", "name": "run", "arguments": "{}"}`,
		`{"type": "function_call_output", "call_id": "call_1", "output": "done"}`,
	} {
		if !bytes.Contains(f.last().body, []byte(item)) {
			t.Errorf("the upstream did not get %s unchanged:\n%s", item, f.last().body)
		}
	}
}

func TestTheEffortOfTheLastConfigurationUpdateBecomesTheRequestsEffort(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	postResponses(f, strings.NewReader(responsesRequestWithUnsupportedParts), nil)
	got := decodeObject(t, f.last().body)
	if want := json.RawMessage(`{"effort":"xhigh","summary":"auto"}`); !jsonEqual(t, got["reasoning"], want) {
		t.Errorf("reasoning = %s, want %s", got["reasoning"], want)
	}
}

func TestAnEffortIsAddedWhenTheRequestHasNoReasoning(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	postResponses(f, strings.NewReader(`{"model":"gpt-x","input":[{"type":"configuration_update","reasoning":{"effort":"low"}}]}`), nil)
	got := decodeObject(t, f.last().body)
	if want := json.RawMessage(`{"effort":"low"}`); !jsonEqual(t, got["reasoning"], want) {
		t.Errorf("reasoning = %s, want %s", got["reasoning"], want)
	}
	if types := inputTypes(t, got["input"]); len(types) != 0 {
		t.Errorf("input items = %v, want none", types)
	}
}

func TestAConfigurationUpdateWithoutAnEffortLeavesTheReasoningAlone(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	postResponses(f, strings.NewReader(`{"model":"gpt-x","reasoning":{"effort":"high"},"input":[{"type":"configuration_update"}]}`), nil)
	got := decodeObject(t, f.last().body)
	if want := json.RawMessage(`{"effort":"high"}`); !jsonEqual(t, got["reasoning"], want) {
		t.Errorf("reasoning = %s, want %s", got["reasoning"], want)
	}
	if !strings.Contains(f.logs.String(), `"moved_fields":[]`) {
		t.Errorf("logs do not say nothing was moved:\n%s", f.logs.String())
	}
}

func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &y); err != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}

func TestARequestWithNothingToDropIsSentByteForByte(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	body := "{ \"model\":\"gpt-x\",\n \"input\":[{\"type\":\"reasoning\",\"encrypted_content\":\"opaque\"}],\"prompt_cache_key\":\"k1\" }\n"
	postResponses(f, strings.NewReader(body), nil)
	if got := string(f.last().body); got != body {
		t.Errorf("body = %q, want it unchanged", got)
	}
}

func TestAStringInputIsKept(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	postResponses(f, strings.NewReader(`{"model":"gpt-x","input":"hello","temperature":1}`), nil)
	got := decodeObject(t, f.last().body)
	if string(got["input"]) != `"hello"` {
		t.Errorf("input = %s", got["input"])
	}
	if _, ok := got["temperature"]; ok {
		t.Error("temperature reached the upstream")
	}
}

func TestTheContentLengthMatchesTheBodySent(t *testing.T) {
	var contentLength int64
	f := newFixture(t, options{verifier: fakeVerifier{}, handler: func(w http.ResponseWriter, r *http.Request) {
		contentLength = r.ContentLength
		w.WriteHeader(http.StatusOK)
	}})
	postResponses(f, strings.NewReader(responsesRequestWithUnsupportedParts), nil)
	if got := int64(len(f.last().body)); contentLength != got {
		t.Errorf("Content-Length = %d, body is %d bytes", contentLength, got)
	}
}

func TestOnlyTheNamesOfWhatWasDroppedAreLoggedInOneLine(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	postResponses(f, strings.NewReader(responsesRequestWithUnsupportedParts), nil)

	logs := f.logs.String()
	for _, value := range []string{"24h", "30m", "32000", "0.2", "xhigh", "opaque-reasoning", "hello"} {
		if strings.Contains(logs, value) {
			t.Errorf("logs carry the value %q:\n%s", value, logs)
		}
	}
	var dropped []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("not a JSON line: %q", line)
		}
		if _, ok := entry["dropped_fields"]; ok {
			dropped = append(dropped, entry)
		}
	}
	if len(dropped) != 1 {
		t.Fatalf("got %d lines naming what was dropped, want 1:\n%s", len(dropped), logs)
	}
	entry := dropped[0]
	if entry["path"] != "/v1/responses" || entry["subject"] != subject {
		t.Errorf("entry = %v", entry)
	}
	if got := strings2(entry["dropped_fields"]); !slices.Equal(got, []string{"max_output_tokens", "prompt_cache_options", "prompt_cache_retention", "temperature"}) {
		t.Errorf("dropped_fields = %v", got)
	}
	if got := strings2(entry["dropped_input_items"]); !slices.Equal(got, []string{"configuration_update"}) {
		t.Errorf("dropped_input_items = %v", got)
	}
	if got := strings2(entry["moved_fields"]); !slices.Equal(got, []string{"reasoning.effort"}) {
		t.Errorf("moved_fields = %v", got)
	}
}

func strings2(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, s := range list {
		str, _ := s.(string)
		out = append(out, str)
	}
	return out
}

func TestNothingIsLoggedAboutDroppingWhenNothingWasDropped(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	postResponses(f, strings.NewReader(`{"model":"gpt-x","input":"hello"}`), nil)
	if strings.Contains(f.logs.String(), "dropped_fields") {
		t.Errorf("logs name a drop:\n%s", f.logs.String())
	}
}

func TestABodyThatIsNotAJSONObjectIsSentUnchanged(t *testing.T) {
	for _, body := range []string{
		"not json at all",
		`["temperature"]`,
		`{"temperature": 1, "input": [`,
		`{"temperature": 1} trailing`,
		``,
	} {
		f := newFixture(t, options{verifier: fakeVerifier{}})
		resp := postResponses(f, strings.NewReader(body), nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%q: status = %d", body, resp.StatusCode)
			continue
		}
		if got := string(f.last().body); got != body {
			t.Errorf("body = %q, want %q unchanged", got, body)
		}
	}
}

func TestACompressedRequestBodyIsSentUnchanged(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = io.WriteString(zw, `{"model":"gpt-x","temperature":1}`)
	_ = zw.Close()

	f := newFixture(t, options{verifier: fakeVerifier{}})
	postResponses(f, bytes.NewReader(compressed.Bytes()), http.Header{"Content-Encoding": {"gzip"}})
	got := f.last()
	if !bytes.Equal(got.body, compressed.Bytes()) {
		t.Error("the compressed body was changed")
	}
	if got.header.Get("Content-Encoding") != "gzip" {
		t.Errorf("Content-Encoding = %q", got.header.Get("Content-Encoding"))
	}
}

func TestARequestBodyOverTheLimitIsRefused(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}})
	body := `{"input":"` + strings.Repeat("a", MaxRequestBody) + `"}`
	resp := postResponses(f, strings.NewReader(body), nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
	if n := f.received(); n != 0 {
		t.Errorf("the upstream received %d requests", n)
	}
}

func TestOtherRoutesAreNotRead(t *testing.T) {
	f := newFixture(t, options{verifier: fakeVerifier{}, routes: "POST /v1/responses,POST /v1/other"})
	body := `{"temperature":1,"input":[{"type":"configuration_update"}]}`
	f.do(http.MethodPost, "/v1/other", strings.NewReader(body), withAssertion(http.Header{"Content-Type": {"application/json"}}))
	if got := string(f.last().body); got != body {
		t.Errorf("body = %q, want it unchanged", got)
	}
}
