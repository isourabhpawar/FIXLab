package websocket

import (
	"sync"
	"testing"
	"time"
)

func testEvent(sessionID, typ string) Event {
	return Event{Type: typ, SessionID: sessionID, Timestamp: time.Now(), Payload: map[string]string{"k": "v"}}
}

func TestHub_PublishSubscribe(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe("sess-1")
	defer unsub()

	h.Publish(testEvent("sess-1", EventFIXMsgIn))
	h.Publish(testEvent("sess-2", EventFIXMsgIn)) // different session: must not arrive

	select {
	case ev := <-ch:
		if ev.Type != EventFIXMsgIn || ev.SessionID != "sess-1" {
			t.Fatalf("unexpected event %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("expected event not received")
	}
	select {
	case ev := <-ch:
		t.Fatalf("unexpected cross-session event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHub_Unsubscribe(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe("sess-1")
	unsub()
	h.Publish(testEvent("sess-1", EventFIXMsgIn))
	select {
	case ev := <-ch:
		t.Fatalf("received event after unsubscribe: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHub_Close(t *testing.T) {
	h := NewHub()
	ch1, unsub1 := h.Subscribe("sess-1")
	defer unsub1()
	ch2, unsub2 := h.Subscribe("sess-1")
	defer unsub2()

	h.Close("sess-1")
	for i, ch := range []<-chan Event{ch1, ch2} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Fatalf("channel %d: expected close", i)
			}
		case <-time.After(time.Second):
			t.Fatalf("channel %d: not closed", i)
		}
	}
	// Publishing after close must not panic.
	h.Publish(testEvent("sess-1", EventFIXMsgIn))
}

func TestHub_SlowSubscriberDrops(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe("sess-1")
	defer unsub()

	for i := 0; i < subscriberBuffer+100; i++ {
		h.Publish(testEvent("sess-1", EventFIXMsgIn))
	}
	if got := h.Dropped("sess-1"); got != 100 {
		t.Fatalf("dropped = %d, want 100", got)
	}
	// Publish must never block even with a full subscriber.
	done := make(chan struct{})
	go func() { h.Publish(testEvent("sess-1", EventFIXMsgIn)); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on slow subscriber")
	}
	// The buffered events are still intact.
	n := 0
	for len(ch) > 0 {
		<-ch
		n++
	}
	if n != subscriberBuffer {
		t.Fatalf("buffered events = %d, want %d", n, subscriberBuffer)
	}
}

func TestHub_ConcurrentPublish(t *testing.T) {
	h := NewHub()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		ch, unsub := h.Subscribe("sess-1")
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer unsub()
			for range ch {
			}
		}()
	}
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.Publish(testEvent("sess-1", EventFIXMsgIn))
		}(i)
	}
	h.Close("sess-1") // lets the ranging subscribers exit
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent publish/subscribe hung")
	}
}

func TestNoopHub(t *testing.T) {
	var h Hub = NoopHub{}
	ch, unsub := h.Subscribe("x")
	unsub()
	h.Publish(testEvent("x", EventFIXMsgIn))
	h.Close("x")
	if h.Dropped("x") != 0 {
		t.Fatal("NoopHub dropped != 0")
	}
	select {
	case <-ch:
		t.Fatal("NoopHub delivered an event")
	default:
	}
}
