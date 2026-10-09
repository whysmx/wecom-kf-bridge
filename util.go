package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const DefaultChunkBytes = 2000

// ChunkUTF8 splits text without cutting a UTF-8 code point. Invalid UTF-8 is
// rejected rather than silently replaced, since protocol payloads are UTF-8.
func ChunkUTF8(text string, maxBytes int) ([]string, error) {
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("bridge: invalid UTF-8 text")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultChunkBytes
	}
	out := make([]string, 0, (len(text)/maxBytes)+1)
	for len(text) > 0 {
		n := len(text)
		if n > maxBytes {
			n = maxBytes
		}
		for n > 0 && !utf8.ValidString(text[:n]) {
			n--
		}
		if n == 0 {
			return nil, fmt.Errorf("bridge: chunk boundary is not UTF-8")
		}
		out = append(out, text[:n])
		text = text[n:]
	}
	return out, nil
}

// SplitUTF8 is retained as a concise alias for callers of the protocol layer.
func SplitUTF8(text string, maxBytes int) ([]string, error) { return ChunkUTF8(text, maxBytes) }

func validUserID(id string) bool { return id != "" && !strings.ContainsAny(id, "@,| \t\r\n") }
func validAgentID(id string) bool {
	if id == "" {
		return false
	}
	_, err := strconv.ParseUint(id, 10, 64)
	return err == nil
}

// parseAgentID accepts the two forms emitted by cc-connect while rejecting
// floating point values and silently truncated JSON numbers.
func parseAgentID(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", fmt.Errorf("missing agentid")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		if !validAgentID(s) {
			return "", fmt.Errorf("invalid agentid")
		}
		return s, nil
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil || n.String() == "" || strings.ContainsAny(n.String(), ".eE") {
		return "", fmt.Errorf("invalid agentid")
	}
	if !validAgentID(n.String()) {
		return "", fmt.Errorf("invalid agentid")
	}
	return n.String(), nil
}

func sameAgentID(a, b string) bool {
	if !validAgentID(a) || !validAgentID(b) {
		return false
	}
	x, e1 := strconv.ParseUint(a, 10, 64)
	y, e2 := strconv.ParseUint(b, 10, 64)
	return e1 == nil && e2 == nil && x == y
}
