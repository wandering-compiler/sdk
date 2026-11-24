package runner

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// A tool refusal's detail lines reach the CallError, so `expect_error.details`
// works on MCP as it does on REST. The text is what lib/mcp toolErrorResult
// writes: the code and message, then one line per ErrorDetail, whose field
// and code may each be absent.
func TestMCPCall_RefusalCarriesItsDetails(t *testing.T) {
	text := "InvalidArgument: validation failed\n" +
		"- cursor: INVALID_VALUE — the cursor belongs to another query\n" +
		"- items[0].name: TOO_LONG — at most 64 characters: got 70\n" +
		"- REQUIRED — a value is required\n" +
		"- limit: OUT_OF_RANGE\n" +
		"- a message with no field or code"
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "result": map[string]any{
		"isError": true, "content": []map[string]any{{"type": "text", "text": text}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	caller, _ := newMCPStub(t, &mcpStub{sessionHeader: "s", callBody: string(body)})
	_, err = caller.Call(context.Background(), mcpEndpoint(), nil, "", nil, "", nil)
	ce, ok := err.(*CallError)
	if !ok {
		t.Fatalf("want a CallError, got %v", err)
	}
	if ce.Code != "INVALID_ARGUMENT" || ce.Message != "validation failed" {
		t.Errorf("code %q message %q", ce.Code, ce.Message)
	}
	want := []CallErrorDetail{
		{Field: "cursor", Code: "INVALID_VALUE", Message: "the cursor belongs to another query"},
		{Field: "items[0].name", Code: "TOO_LONG", Message: "at most 64 characters: got 70"},
		{Code: "REQUIRED", Message: "a value is required"},
		{Field: "limit", Code: "OUT_OF_RANGE"},
		{Message: "a message with no field or code"},
	}
	if !reflect.DeepEqual(ce.Details, want) {
		t.Errorf("details:\n got %+v\nwant %+v", ce.Details, want)
	}
}

// A refusal without details keeps an empty list, as on REST.
func TestMCPCall_RefusalWithoutDetails(t *testing.T) {
	caller, _ := newMCPStub(t, &mcpStub{sessionHeader: "s",
		callBody: `{"jsonrpc":"2.0","id":2,"result":{"isError":true,"content":[{"type":"text","text":"NotFound: Not found."}]}}`})
	_, err := caller.Call(context.Background(), mcpEndpoint(), nil, "", nil, "", nil)
	if ce, ok := err.(*CallError); !ok || ce.Code != "NOT_FOUND" || len(ce.Details) != 0 {
		t.Errorf("got %#v", err)
	}
}
