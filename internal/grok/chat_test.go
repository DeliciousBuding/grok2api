package grok

import "testing"

func TestStreamAdapterSkipsEmptySuccessfulPayload(t *testing.T) {
	adapter := NewStreamAdapter()

	events, errObj := adapter.Feed([]byte(`{"result":{}}`))

	if errObj != nil {
		t.Fatalf("empty successful payload should not be an error: %v", errObj)
	}
	if len(events) != 0 {
		t.Fatalf("empty successful payload should emit no events, got %#v", events)
	}
}

func TestStreamAdapterSkipsMalformedFrame(t *testing.T) {
	adapter := NewStreamAdapter()

	events, errObj := adapter.Feed([]byte(`{"result":`))

	if errObj != nil {
		t.Fatalf("malformed frame should not be fatal: %v", errObj)
	}
	if len(events) != 0 {
		t.Fatalf("malformed frame should emit no events, got %#v", events)
	}
}
