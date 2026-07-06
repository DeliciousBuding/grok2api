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

func TestEmitImagineEventReturnsOnCanceledContextWithFullChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan ImagineEvent, 1)
	ch <- ImagineEvent{Type: ImagineEventProgress}
	cancel()

	done := make(chan bool, 1)
	go func() {
		done <- emitImagineEvent(ctx, ch, ImagineEvent{Type: ImagineEventError})
	}()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("expected canceled emit to report false")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected canceled emit to return without blocking on full channel")
	}
}
