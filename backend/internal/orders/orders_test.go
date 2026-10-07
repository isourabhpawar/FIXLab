package orders

import (
	"errors"
	"sync"
	"testing"
)

func mkOrder(id string) *Order {
	return &Order{
		ClOrdID:   id,
		Symbol:    "MSFT",
		Side:      "1",
		OrderQty:  1000,
		Price:     310.50,
		OrdType:   "2",
		LeavesQty: 1000,
		Status:    StatusNew,
	}
}

func TestAddGetUpdate(t *testing.T) {
	s := NewInMemoryStore(10)
	if err := s.Add(mkOrder("A1")); err != nil {
		t.Fatal(err)
	}
	o, ok := s.Get("A1")
	if !ok {
		t.Fatal("expected order A1")
	}
	if o.Symbol != "MSFT" || o.Status != StatusNew {
		t.Fatalf("unexpected order: %+v", o)
	}
	// Mutating the returned copy must not affect the store.
	o.Status = StatusFilled
	o2, _ := s.Get("A1")
	if o2.Status != StatusNew {
		t.Fatal("store leaked a mutable reference")
	}
	o2.Status = StatusPartiallyFilled
	o2.CumQty = 400
	o2.LeavesQty = 600
	if err := s.Update(o2); err != nil {
		t.Fatal(err)
	}
	o3, _ := s.Get("A1")
	if o3.Status != StatusPartiallyFilled || o3.CumQty != 400 {
		t.Fatalf("update not applied: %+v", o3)
	}
	if s.Count() != 1 {
		t.Fatalf("count = %d, want 1", s.Count())
	}
}

func TestDuplicateAndCap(t *testing.T) {
	s := NewInMemoryStore(2)
	if err := s.Add(mkOrder("A1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(mkOrder("A1")); !errors.Is(err, ErrOrderExists) {
		t.Fatalf("want ErrOrderExists, got %v", err)
	}
	if err := s.Add(mkOrder("A2")); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(mkOrder("A3")); !errors.Is(err, ErrOrderCapExceeded) {
		t.Fatalf("want ErrOrderCapExceeded, got %v", err)
	}
}

func TestUpdateMissing(t *testing.T) {
	s := NewInMemoryStore(10)
	if err := s.Update(mkOrder("NOPE")); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("want ErrOrderNotFound, got %v", err)
	}
}

func TestClear(t *testing.T) {
	s := NewInMemoryStore(10)
	_ = s.Add(mkOrder("A1"))
	s.Clear()
	if s.Count() != 0 {
		t.Fatal("clear did not empty the store")
	}
	if _, ok := s.Get("A1"); ok {
		t.Fatal("order survived Clear")
	}
}

func TestStatusSets(t *testing.T) {
	working := []Status{StatusNew, StatusPartiallyFilled, StatusReplaced}
	for _, st := range working {
		if !st.Working() || st.Terminal() {
			t.Fatalf("%s should be working", st)
		}
	}
	terminal := []Status{StatusFilled, StatusRejected, StatusCanceled}
	for _, st := range terminal {
		if !st.Terminal() || st.Working() {
			t.Fatalf("%s should be terminal", st)
		}
	}
	if StatusPendingCancel.Working() || StatusPendingCancel.Terminal() {
		t.Fatal("PENDING_CANCEL is neither working nor terminal")
	}
}

func TestConcurrentAdd(t *testing.T) {
	s := NewInMemoryStore(1000)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.Add(mkOrder("C" + string(rune('0'+i/10)) + string(rune('0'+i%10))))
		}(i)
	}
	wg.Wait()
	if s.Count() != 50 {
		t.Fatalf("count = %d, want 50", s.Count())
	}
}
