package grok

import (
	"context"
	"testing"
	"time"
)

func TestImagineStreamHonorsCanceledContextBeforeDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stream := NewImagineStream("tok-test")
	events := stream.StreamImages(ctx, "prompt", "1:1", 1, false, false)

	select {
	case ev, ok := <-events:
		if ok {
			t.Fatalf("expected canceled stream to close without events, got %#v", ev)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected canceled stream to close promptly")
	}
}
