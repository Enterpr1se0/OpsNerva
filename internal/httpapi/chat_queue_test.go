package httpapi

import (
	"errors"
	"fmt"
	"testing"

	"eino-ops-agent/internal/domain"
)

func TestChatMessageQueueAdvancesAfterTurnsInOrder(t *testing.T) {
	queue := newChatMessageQueue()
	queueCtx, started := queue.begin("session-test")
	if !started {
		t.Fatal("queue did not start")
	}
	if _, duplicate := queue.begin("session-test"); duplicate {
		t.Fatal("queue did not enforce one active driver")
	}
	first, position, err := queue.enqueue("session-test", " first ", []domain.ChatAttachment{{Name: "screen.png", MIMEType: "image/png", SizeBytes: 3, Data: []byte("png")}})
	if err != nil || position != 1 || first.Message != "first" || len(first.Attachments) != 1 || first.Attachments[0].Data != nil {
		t.Fatalf("first queued message = %#v, position = %d, err = %v", first, position, err)
	}
	second, position, err := queue.enqueue("session-test", "second", nil)
	if err != nil || position != 2 {
		t.Fatalf("second queued message = %#v, position = %d, err = %v", second, position, err)
	}
	if snapshot := queue.snapshot("session-test"); len(snapshot) != 2 || snapshot[0].ID != first.ID || snapshot[1].ID != second.ID {
		t.Fatalf("queue snapshot = %#v", snapshot)
	}
	next, ok := queue.nextAfterTurn("session-test")
	if !ok || next.ID != first.ID || string(next.Attachments[0].Data) != "png" {
		t.Fatalf("first consumed message = %#v, ok = %t", next, ok)
	}
	next, ok = queue.nextAfterTurn("session-test")
	if !ok || next.ID != second.ID {
		t.Fatalf("second consumed message = %#v, ok = %t", next, ok)
	}
	if _, ok := queue.nextAfterTurn("session-test"); ok || !queue.active("session-test") {
		t.Fatal("draining queue did not retain its driver until the terminal event")
	}
	if _, _, err := queue.enqueue("session-test", "late", nil); !errors.Is(err, errChatQueueInactive) {
		t.Fatalf("draining queue accepted another message: %v", err)
	}
	queue.finish("session-test")
	if queue.active("session-test") || queueCtx.Err() == nil {
		t.Fatal("finished queue did not cancel its driver context")
	}
}

func TestChatMessageQueueIsBoundedAndCancelClearsIt(t *testing.T) {
	queue := newChatMessageQueue()
	if _, _, err := queue.enqueue("missing", "message", nil); !errors.Is(err, errChatQueueInactive) {
		t.Fatalf("inactive queue error = %v", err)
	}
	queueCtx, started := queue.begin("session-test")
	if !started {
		t.Fatal("queue did not start")
	}
	for index := 0; index < maxQueuedChatMessages; index++ {
		if _, _, err := queue.enqueue("session-test", fmt.Sprintf("message %d", index), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := queue.enqueue("session-test", "overflow", nil); !errors.Is(err, errChatQueueFull) {
		t.Fatalf("full queue error = %v", err)
	}
	if cleared, active := queue.clear("session-test"); cleared != maxQueuedChatMessages || !active {
		t.Fatalf("cleared messages = %d", cleared)
	}
	if !queue.active("session-test") {
		t.Fatal("cancelled queue released its driver before the terminal event")
	}
	if queueCtx.Err() == nil {
		t.Fatal("cancelled queue did not cancel its driver context")
	}
	queue.finish("session-test")
	if queue.active("session-test") {
		t.Fatal("finished cancelled queue remained active")
	}
}
