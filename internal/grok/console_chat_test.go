package grok

import "testing"

func TestConsoleStreamAdapterSkipsMalformedFrame(t *testing.T) {
	adapter := NewConsoleStreamAdapter()

	tokens, errObj := adapter.Feed("response.output_text.delta", `{"delta":`)

	if errObj != nil {
		t.Fatalf("malformed console frame should not be fatal: %v", errObj)
	}
	if len(tokens) != 0 {
		t.Fatalf("malformed console frame should emit no tokens, got %#v", tokens)
	}
}

func TestClassifyConsoleLineTreatsDoneAsDone(t *testing.T) {
	kind, value := ClassifyConsoleLine("data: [DONE]")

	if kind != "done" || value != "" {
		t.Fatalf("expected done line, got kind=%q value=%q", kind, value)
	}
}
