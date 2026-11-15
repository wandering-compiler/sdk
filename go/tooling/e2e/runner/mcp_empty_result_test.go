package runner

import (
	"strings"
	"testing"
)

// A tool result that carries neither structuredContent nor a text block is
// not an answer: the w17 MCP gateway always returns the response as JSON
// text (`{}` for a response with no fields). It used to read as an empty
// object, so a step with no expect — which passes only on a response without
// fields — passed against a tool that returned nothing at all, whatever its
// response declares.
func TestUnwrapToolResult_NoContentIsAnError(t *testing.T) {
	_, err := unwrapToolResult("notes.list_notes", &toolResult{})
	if err == nil {
		t.Fatal("a tool result with no content was accepted as an empty response")
	}
	if !strings.Contains(err.Error(), "notes.list_notes") || !strings.Contains(err.Error(), "no content") {
		t.Errorf("refused, but the message does not say what is missing: %v", err)
	}
	r := &toolResult{}
	r.Content = append(r.Content, struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "image", Text: "ignored"})
	if _, err := unwrapToolResult("notes.list_notes", r); err == nil {
		t.Error("a result with only a non-text block was accepted as an empty response")
	}
	// The gateway's answer for a response with no fields still passes.
	r = &toolResult{}
	r.Content = append(r.Content, struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "text", Text: "{}"})
	if out, err := unwrapToolResult("notes.list_notes", r); err != nil || len(out) != 0 {
		t.Errorf("`{}` text = %v, %v; want an empty object", out, err)
	}
}
