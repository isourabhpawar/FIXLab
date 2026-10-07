package simulator

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/quickfixgo/quickfix"

	"fixlab.dev/fixlab/backend/internal/messages"
	"fixlab.dev/fixlab/backend/internal/orders"
)

// fakeSender implements Sender for tests: it records outbound messages
// and published events instead of touching a FIX engine.
type fakeSender struct {
	seq     atomic.Int64
	mu      sync.Mutex
	sent    []*quickfix.Message
	created []*orders.Order
	updated []*orders.Order
	execs   []*ExecutionSummary
	quota   bool // when true, SendExecution fails with QuotaExceededError
}

func (f *fakeSender) NextExecSeq() int64 { return f.seq.Add(1) }

func (f *fakeSender) SendExecution(msg *quickfix.Message) error {
	if f.quota {
		return &QuotaExceededError{Limit: 250}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeSender) PublishOrderEvent(eventType string, o *orders.Order) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *o
	if eventType == "ORDER_CREATED" {
		f.created = append(f.created, &cp)
	} else {
		f.updated = append(f.updated, &cp)
	}
}

func (f *fakeSender) PublishExecutionSent(sum *ExecutionSummary) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, sum)
}

func newTestSim(t *testing.T) (Simulator, *fakeSender, orders.Store) {
	t.Helper()
	store := orders.NewInMemoryStore(1000)
	sender := &fakeSender{}
	sim, err := New(Config{Store: store, Sender: sender})
	if err != nil {
		t.Fatal(err)
	}
	return sim, sender, store
}

func rec(msgType string, fields ...messages.Field) *messages.Record {
	return &messages.Record{MsgType: msgType, Fields: fields}
}

func f(tag int, v string) messages.Field { return messages.Field{Tag: tag, Value: v} }

func bodyStr(m *quickfix.Message, tag quickfix.Tag) string {
	v, err := m.Body.GetString(tag)
	if err != nil {
		return "<missing>"
	}
	return v
}

func sendNOS(t *testing.T, sim Simulator, clOrdID string) {
	t.Helper()
	err := sim.OnAppMessage(rec("D",
		f(11, clOrdID), f(55, "MSFT"), f(54, "1"),
		f(38, "1000"), f(40, "2"), f(44, "310.50"),
	))
	if err != nil {
		t.Fatalf("OnAppMessage(D): %v", err)
	}
}

func TestNewOrderCreatesOrderAndAck(t *testing.T) {
	sim, sender, store := newTestSim(t)
	sendNOS(t, sim, "ORD1")

	o, ok := store.Get("ORD1")
	if !ok {
		t.Fatal("order not created")
	}
	if o.Status != orders.StatusNew || o.LeavesQty != 1000 || o.CumQty != 0 {
		t.Fatalf("bad order state: %+v", o)
	}
	if o.OrderID == "" || !strings.HasPrefix(o.OrderID, "FLB") {
		t.Fatalf("bad OrderID: %q", o.OrderID)
	}
	if len(sender.created) != 1 {
		t.Fatalf("ORDER_CREATED published %d times", len(sender.created))
	}
	if len(sender.sent) != 1 {
		t.Fatalf("expected 1 ack ER, got %d", len(sender.sent))
	}
	ack := sender.sent[0]
	mt, _ := ack.Header.GetString(tagMsgType)
	if mt != "8" {
		t.Fatalf("ack msg type = %q, want 8", mt)
	}
	if got := bodyStr(ack, tagExecType); got != ExecTypeNew {
		t.Fatalf("150 = %q, want 0", got)
	}
	if got := bodyStr(ack, tagOrdStatus); got != OrdStatusNew {
		t.Fatalf("39 = %q, want 0", got)
	}
	if got := bodyStr(ack, tagLeavesQty); got != "1000" {
		t.Fatalf("151 = %q, want 1000", got)
	}
	if got := bodyStr(ack, tagClOrdID); got != "ORD1" {
		t.Fatalf("11 = %q, want ORD1", got)
	}
	if len(sender.execs) != 1 || sender.execs[0].MsgType != "8" {
		t.Fatal("EXECUTION_SENT not published for the ack")
	}
}

func TestNewOrderDuplicate(t *testing.T) {
	sim, _, _ := newTestSim(t)
	sendNOS(t, sim, "ORD1")
	err := sim.OnAppMessage(rec("D",
		f(11, "ORD1"), f(55, "MSFT"), f(54, "1"), f(38, "100"), f(40, "2")))
	if err == nil {
		t.Fatal("expected duplicate ClOrdID error")
	}
}

func TestNewOrderBadQty(t *testing.T) {
	sim, _, _ := newTestSim(t)
	err := sim.OnAppMessage(rec("D",
		f(11, "BAD"), f(55, "MSFT"), f(54, "1"), f(38, "0"), f(40, "2")))
	var ie *InputError
	if !errors.As(err, &ie) {
		t.Fatalf("want InputError, got %v", err)
	}
}

func TestPartialAndFullFillMath(t *testing.T) {
	sim, sender, _ := newTestSim(t)
	sendNOS(t, sim, "ORD1")

	// Partial: 400 @ 310.50
	res, err := sim.PartialFill("ORD1", 400, 310.50)
	if err != nil {
		t.Fatal(err)
	}
	o := res.Order
	if o.CumQty != 400 || o.LeavesQty != 600 || o.Status != orders.StatusPartiallyFilled {
		t.Fatalf("after partial: %+v", o)
	}
	if o.AvgPx != 310.50 {
		t.Fatalf("avgPx = %v, want 310.50", o.AvgPx)
	}
	er := sender.sent[len(sender.sent)-1]
	if got := bodyStr(er, tagExecType); got != ExecTypePartialFill {
		t.Fatalf("150 = %q, want D", got)
	}
	if got := bodyStr(er, tagOrdStatus); got != OrdStatusPartiallyFilled {
		t.Fatalf("39 = %q, want 1", got)
	}
	if got := bodyStr(er, tagCumQty); got != "400" {
		t.Fatalf("14 = %q, want 400", got)
	}
	if got := bodyStr(er, tagLeavesQty); got != "600" {
		t.Fatalf("151 = %q, want 600", got)
	}

	// Full: 600 @ 310.60 -> weighted avg (400*310.50 + 600*310.60)/1000
	res, err = sim.Fill("ORD1", 600, 310.60)
	if err != nil {
		t.Fatal(err)
	}
	o = res.Order
	if o.CumQty != 1000 || o.LeavesQty != 0 || o.Status != orders.StatusFilled {
		t.Fatalf("after fill: %+v", o)
	}
	wantAvg := (400*310.50 + 600*310.60) / 1000
	if o.AvgPx != wantAvg {
		t.Fatalf("avgPx = %v, want %v", o.AvgPx, wantAvg)
	}
	er = sender.sent[len(sender.sent)-1]
	if got := bodyStr(er, tagExecType); got != ExecTypeFill {
		t.Fatalf("150 = %q, want F", got)
	}
	if got := bodyStr(er, tagOrdStatus); got != OrdStatusFilled {
		t.Fatalf("39 = %q, want 2", got)
	}
	if got := bodyStr(er, tagLastQty); got != "600" {
		t.Fatalf("32 = %q, want 600", got)
	}
	if got := bodyStr(er, tagLastPx); got != "310.6" {
		t.Fatalf("31 = %q, want 310.6", got)
	}
	if got := bodyStr(er, tagAvgPx); got != "310.56" {
		t.Fatalf("6 = %q, want 310.56", got)
	}

	// Filling a filled order is a state error.
	if _, err := sim.Fill("ORD1", 1, 1); err == nil {
		t.Fatal("expected error filling a FILLED order")
	} else {
		var se *StateError
		if !errors.As(err, &se) {
			t.Fatalf("want StateError, got %T", err)
		}
	}
}

func TestConcurrentPartialFills(t *testing.T) {
	sim, _, store := newTestSim(t)
	sendNOS(t, sim, "ORD1")
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = sim.PartialFill("ORD1", 100, 310.50)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("partial fill failed: %v", err)
		}
	}
	o, _ := store.Get("ORD1")
	if o.CumQty != 1000 || o.LeavesQty != 0 || o.Status != orders.StatusFilled {
		t.Fatalf("concurrent fills double-counted: %+v", o)
	}
}

func TestFillQtyMustEqualLeaves(t *testing.T) {
	sim, _, _ := newTestSim(t)
	sendNOS(t, sim, "ORD1")
	if _, err := sim.Fill("ORD1", 500, 310.50); err == nil {
		t.Fatal("expected InputError for FILL qty != leaves")
	} else {
		var ie *InputError
		if !errors.As(err, &ie) {
			t.Fatalf("want InputError, got %T", err)
		}
	}
}

func TestReject(t *testing.T) {
	sim, sender, store := newTestSim(t)
	sendNOS(t, sim, "ORD1")
	res, err := sim.Reject("ORD1", "2", "Exchange closed")
	if err != nil {
		t.Fatal(err)
	}
	if res.Order.Status != orders.StatusRejected || res.Order.LeavesQty != 0 {
		t.Fatalf("bad reject state: %+v", res.Order)
	}
	er := sender.sent[len(sender.sent)-1]
	if got := bodyStr(er, tagExecType); got != ExecTypeRejected {
		t.Fatalf("150 = %q, want 8", got)
	}
	if got := bodyStr(er, tagOrdStatus); got != OrdStatusRejected {
		t.Fatalf("39 = %q, want 8", got)
	}
	if got := bodyStr(er, tagOrdRejReason); got != "2" {
		t.Fatalf("103 = %q, want 2", got)
	}
	if got := bodyStr(er, tagText); got != "Exchange closed" {
		t.Fatalf("58 = %q", got)
	}
	_ = store
	// Reject without reason or text is an input error.
	sendNOS(t, sim, "ORD2")
	if _, err := sim.Reject("ORD2", "", ""); err == nil {
		t.Fatal("expected InputError for reason-less reject")
	}
}

func TestCancelAcceptFlow(t *testing.T) {
	sim, sender, store := newTestSim(t)
	sendNOS(t, sim, "ORD1")

	err := sim.OnAppMessage(rec("F", f(11, "CX1"), f(41, "ORD1"), f(54, "1"), f(38, "1000")))
	if err != nil {
		t.Fatalf("OnAppMessage(F): %v", err)
	}
	o, _ := store.Get("ORD1")
	if o.Status != orders.StatusPendingCancel {
		t.Fatalf("status = %s, want PENDING_CANCEL", o.Status)
	}
	if len(sender.updated) == 0 {
		t.Fatal("ORDER_UPDATED not published for cancel request")
	}

	res, err := sim.CancelAccept("ORD1", "ok")
	if err != nil {
		t.Fatal(err)
	}
	if res.Order.Status != orders.StatusCanceled {
		t.Fatalf("status = %s, want CANCELED", res.Order.Status)
	}
	er := sender.sent[len(sender.sent)-1]
	if got := bodyStr(er, tagExecType); got != ExecTypeCanceled {
		t.Fatalf("150 = %q, want 4", got)
	}
	if got := bodyStr(er, tagOrdStatus); got != OrdStatusCanceled {
		t.Fatalf("39 = %q, want 4", got)
	}
}

func TestCancelRejectFlow(t *testing.T) {
	sim, sender, store := newTestSim(t)
	sendNOS(t, sim, "ORD1")
	_ = sim.OnAppMessage(rec("F", f(11, "CX1"), f(41, "ORD1"), f(54, "1"), f(38, "1000")))

	res, err := sim.CancelReject("ORD1", "1", "too late")
	if err != nil {
		t.Fatal(err)
	}
	if res.Order.Status != orders.StatusNew {
		t.Fatalf("status = %s, want NEW (restored)", res.Order.Status)
	}
	msg := sender.sent[len(sender.sent)-1]
	mt, _ := msg.Header.GetString(tagMsgType)
	if mt != "9" {
		t.Fatalf("msg type = %q, want 9", mt)
	}
	if got := bodyStr(msg, tagClOrdID); got != "CX1" {
		t.Fatalf("11 = %q, want CX1", got)
	}
	if got := bodyStr(msg, tagOrigClOrdID); got != "ORD1" {
		t.Fatalf("41 = %q, want ORD1", got)
	}
	if got := bodyStr(msg, tagCxlRejResponseTo); got != CxlRejResponseToCancel {
		t.Fatalf("434 = %q, want 1", got)
	}
	if got := bodyStr(msg, tagCxlRejReason); got != "1" {
		t.Fatalf("102 = %q, want 1", got)
	}
	_ = store
}

func TestReplaceAcceptFlow(t *testing.T) {
	sim, sender, _ := newTestSim(t)
	sendNOS(t, sim, "ORD1")

	err := sim.OnAppMessage(rec("G",
		f(11, "RP1"), f(41, "ORD1"), f(54, "1"),
		f(38, "1200"), f(40, "2"), f(44, "311.00")))
	if err != nil {
		t.Fatalf("OnAppMessage(G): %v", err)
	}
	// A fill on the order is blocked while the replace is pending.
	if _, err := sim.Fill("ORD1", 1000, 310.50); err == nil {
		t.Fatal("expected fill to be blocked by pending replace")
	}

	res, err := sim.ReplaceAccept("ORD1")
	if err != nil {
		t.Fatal(err)
	}
	o := res.Order
	if o.OrderQty != 1200 || o.Price != 311 || o.LeavesQty != 1200 || o.Status != orders.StatusReplaced {
		t.Fatalf("bad replaced state: %+v", o)
	}
	er := sender.sent[len(sender.sent)-1]
	if got := bodyStr(er, tagExecType); got != ExecTypeReplaced {
		t.Fatalf("150 = %q, want 5", got)
	}
	if got := bodyStr(er, tagClOrdID); got != "RP1" {
		t.Fatalf("11 = %q, want RP1", got)
	}
	if got := bodyStr(er, tagOrigClOrdID); got != "ORD1" {
		t.Fatalf("41 = %q, want ORD1", got)
	}
	if got := bodyStr(er, tagOrderQty); got != "1200" {
		t.Fatalf("38 = %q, want 1200", got)
	}
	if got := bodyStr(er, tagPrice); got != "311" {
		t.Fatalf("44 = %q, want 311", got)
	}
	_ = sender
}

func TestReplaceRejectFlow(t *testing.T) {
	sim, sender, store := newTestSim(t)
	sendNOS(t, sim, "ORD1")
	_ = sim.OnAppMessage(rec("G",
		f(11, "RP1"), f(41, "ORD1"), f(54, "1"), f(38, "1200"), f(40, "2")))

	res, err := sim.ReplaceReject("ORD1", "2", "nope")
	if err != nil {
		t.Fatal(err)
	}
	o := res.Order
	if o.OrderQty != 1000 || o.PendingReplace != nil {
		t.Fatalf("order terms changed on replace-reject: %+v", o)
	}
	msg := sender.sent[len(sender.sent)-1]
	mt, _ := msg.Header.GetString(tagMsgType)
	if mt != "9" {
		t.Fatalf("msg type = %q, want 9", mt)
	}
	if got := bodyStr(msg, tagCxlRejResponseTo); got != CxlRejResponseToReplace {
		t.Fatalf("434 = %q, want 2", got)
	}
	_ = store
}

func TestQuotaExceeded(t *testing.T) {
	sim, sender, _ := newTestSim(t)
	sendNOS(t, sim, "ORD1")
	sender.quota = true
	_, err := sim.PartialFill("ORD1", 100, 310.50)
	var qe *QuotaExceededError
	if !errors.As(err, &qe) {
		t.Fatalf("want QuotaExceededError, got %v", err)
	}
}

func TestUnknownOrder(t *testing.T) {
	sim, _, _ := newTestSim(t)
	if _, err := sim.Fill("NOPE", 100, 1); !IsNotFound(err) {
		t.Fatalf("want not-found, got %v", err)
	}
	if _, err := sim.CancelAccept("NOPE", ""); !IsNotFound(err) {
		t.Fatalf("want not-found, got %v", err)
	}
}
