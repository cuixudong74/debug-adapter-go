package debug

import (
	"context"
	"testing"
	"time"
)

func TestQueueBasic(t *testing.T) {
	q := NewEventQueue(10)
	q.Emit(&Event{Kind: EvStopped, TaskID: 1})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e, err := q.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != EvStopped {
		t.Fatalf("got %s", e.Kind)
	}
}

func TestQueueEmitAfterNext(t *testing.T) {
	q := NewEventQueue(10)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		time.Sleep(10 * time.Millisecond)
		q.Emit(&Event{Kind: EvStopped, TaskID: 1})
	}()
	e, err := q.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != EvStopped {
		t.Fatalf("got %s", e.Kind)
	}
}
