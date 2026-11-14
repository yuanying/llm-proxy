package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
)

// MaxRequestBody is the largest request body the proxy reads to take out
// what the subscription refuses (ADR 0006). A larger one is refused.
const MaxRequestBody = 64 << 20

// subscriptionRoute is the only request whose body is read.
var subscriptionRoute = Route{Method: "POST", Path: "/v1/responses"}

// unsupportedFields are the request fields pi-ai leaves out when it signs in
// with ChatGPT, because the subscription refuses them.
var unsupportedFields = []string{"max_output_tokens", "prompt_cache_options", "prompt_cache_retention", "temperature"}

// configurationUpdate is the input item type the subscription refuses.
const configurationUpdate = "configuration_update"

// adjustment names what was changed in a request. It holds names only,
// never values, so it can be logged (ADR 0005).
type adjustment struct {
	droppedFields     []string
	droppedInputItems []string
	movedFields       []string
}

func (a adjustment) changed() bool {
	return len(a.droppedFields)+len(a.droppedInputItems)+len(a.movedFields) > 0
}

// member is one "key": value of a JSON object. raw is the member as the
// caller wrote it, from the key's opening quote to the end of the value.
type member struct {
	key   string
	raw   []byte
	value json.RawMessage
}

// adjustForSubscription takes out of a Responses API request what the
// subscription refuses. Everything else is kept as the caller wrote it. A
// body that is not one JSON object, or that has nothing to take out, comes
// back unchanged.
func adjustForSubscription(body []byte) ([]byte, adjustment) {
	var adj adjustment
	members, ok := objectMembers(body)
	if !ok {
		return body, adj
	}

	var effort json.RawMessage
	kept := make([]member, 0, len(members))
	for _, m := range members {
		switch {
		case slices.Contains(unsupportedFields, m.key):
			if !slices.Contains(adj.droppedFields, m.key) {
				adj.droppedFields = append(adj.droppedFields, m.key)
			}
			continue
		case m.key == "input":
			if input, dropped, e, ok := dropConfigurationUpdates(m.value); ok && dropped {
				adj.droppedInputItems = []string{configurationUpdate}
				m = withValue(m, input)
				if e != nil {
					effort = e
				}
			}
		}
		kept = append(kept, m)
	}
	if effort != nil {
		kept = setEffort(kept, effort)
		adj.movedFields = []string{"reasoning.effort"}
	}
	if !adj.changed() {
		return body, adj
	}
	slices.Sort(adj.droppedFields)
	// Every list is written out, empty or not, so that each line has the
	// same fields.
	for _, list := range []*[]string{&adj.droppedFields, &adj.droppedInputItems, &adj.movedFields} {
		if *list == nil {
			*list = []string{}
		}
	}
	return joinObject(kept), adj
}

// dropConfigurationUpdates removes the configuration_update items from an
// input array and returns the effort the last of them carried. ok is false
// when the input is not an array, such as a plain string.
func dropConfigurationUpdates(input json.RawMessage) (out []byte, dropped bool, effort json.RawMessage, ok bool) {
	items, ok := arrayElements(input)
	if !ok {
		return input, false, nil, false
	}
	kept := make([][]byte, 0, len(items))
	for _, item := range items {
		var head struct {
			Type      string `json:"type"`
			Reasoning struct {
				Effort json.RawMessage `json:"effort"`
			} `json:"reasoning"`
		}
		if json.Unmarshal(item, &head) == nil && head.Type == configurationUpdate {
			dropped = true
			if e := head.Reasoning.Effort; len(e) > 0 && !bytes.Equal(e, []byte("null")) {
				effort = e
			}
			continue
		}
		kept = append(kept, item)
	}
	if !dropped {
		return input, false, nil, true
	}
	return joinArray(kept), true, effort, true
}

// setEffort makes the request's reasoning.effort the given one, keeping the
// rest of reasoning as it was.
func setEffort(members []member, effort json.RawMessage) []member {
	effortMember := newMember("effort", effort)
	for i, m := range members {
		if m.key != "reasoning" {
			continue
		}
		inner, ok := objectMembers(m.value)
		if !ok {
			members[i] = newMember("reasoning", joinObject([]member{effortMember}))
			return members
		}
		replaced := false
		for j, im := range inner {
			if im.key == "effort" {
				inner[j] = effortMember
				replaced = true
			}
		}
		if !replaced {
			inner = append(inner, effortMember)
		}
		members[i] = withValue(m, joinObject(inner))
		return members
	}
	return append(members, newMember("reasoning", joinObject([]member{effortMember})))
}

func newMember(key string, value []byte) member {
	k, _ := json.Marshal(key)
	raw := append(append(k, ':'), value...)
	return member{key: key, raw: raw, value: value}
}

func withValue(m member, value []byte) member {
	return newMember(m.key, value)
}

// objectMembers splits a JSON object into its members, keeping each as it
// was written. ok is false when b is not exactly one JSON object.
func objectMembers(b []byte) ([]member, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var members []member
	for dec.More() {
		start := dec.InputOffset()
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tok.(string)
		if !ok {
			return nil, false
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false
		}
		raw := bytes.TrimLeft(b[start:dec.InputOffset()], " \t\r\n,")
		members = append(members, member{key: key, raw: raw, value: value})
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return members, true
}

// arrayElements splits a JSON array into its elements, keeping each as it
// was written. ok is false when b is not a JSON array.
func arrayElements(b []byte) ([][]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil, false
	}
	var elements [][]byte
	for dec.More() {
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false
		}
		elements = append(elements, value)
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim(']') {
		return nil, false
	}
	return elements, true
}

func joinObject(members []member) []byte {
	raws := make([][]byte, len(members))
	for i, m := range members {
		raws[i] = m.raw
	}
	return append(append([]byte{'{'}, bytes.Join(raws, []byte{','})...), '}')
}

func joinArray(elements [][]byte) []byte {
	return append(append([]byte{'['}, bytes.Join(elements, []byte{','})...), ']')
}
