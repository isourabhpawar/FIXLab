package simulator

import (
	"testing"

	"fixlab.dev/fixlab/backend/internal/messages"
	"fixlab.dev/fixlab/backend/internal/orders"
)

// remoteRec builds a messages.Record for an inbound remote report.
func remoteRec(msgType string, fields ...messages.Field) *messages.Record {
	return &messages.Record{
		Type:      "FIX_MESSAGE",
		Direction: messages.Inbound,
		MsgType:   msgType,
		RawFIX:    "raw",
		Fields:    fields,
	}
}

func ff(tag int, v string) messages.Field { return messages.Field{Tag: tag, Value: v} }

func outboundOrder(t *testing.T, store orders.Store, clOrdID string) {
	t.Helper()
	if err := store.Add(&orders.Order{
		OrderID: "FLB1", ClOrdID: clOrdID, Direction: orders.DirectionOutbound,
		Symbol: "T", Side: "1", OrderQty: 100, Price: 50, OrdType: "2",
		LeavesQty: 100, Status: orders.StatusNew,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRemoteFill(t *testing.T) {
	sim, _, store := newTestSim(t)
	outboundOrder(t, store, "O1")
	rec := remoteRec("8",
		ff(11, "O1"), ff(17, "EX1"), ff(37, "R1"), ff(150, "F"), ff(39, "2"),
		ff(54, "1"), ff(32, "100"), ff(31, "50"), ff(14, "100"), ff(151, "0"), ff(6, "50"))
	if err := sim.ApplyRemoteExecutionReport(rec); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	o, _ := store.Get("O1")
	if o.Status != orders.StatusFilled || o.CumQty != 100 || o.LeavesQty != 0 || o.AvgPx != 50 {
		t.Errorf("unexpected order state: %+v", o)
	}
}

func TestApplyRemotePartialThenFill(t *testing.T) {
	sim, _, store := newTestSim(t)
	outboundOrder(t, store, "O2")
	partial := remoteRec("8",
		ff(11, "O2"), ff(150, "D"), ff(39, "1"),
		ff(32, "40"), ff(31, "50"), ff(14, "40"), ff(151, "60"), ff(6, "50"))
	if err := sim.ApplyRemoteExecutionReport(partial); err != nil {
		t.Fatalf("partial apply failed: %v", err)
	}
	o, _ := store.Get("O2")
	if o.Status != orders.StatusPartiallyFilled || o.CumQty != 40 || o.LeavesQty != 60 {
		t.Errorf("unexpected partial state: %+v", o)
	}
}

func TestApplyRemoteCancelConfirm(t *testing.T) {
	sim, _, store := newTestSim(t)
	outboundOrder(t, store, "O3")
	o, _ := store.Get("O3")
	o.Status = orders.StatusPendingCancel
	o.PrevStatus = orders.StatusNew
	o.CancelReqClOrdID = "C9"
	if err := store.Update(o); err != nil {
		t.Fatal(err)
	}
	// 11 is the REQUEST's ClOrdID; 41 names the order.
	rec := remoteRec("8",
		ff(11, "C9"), ff(41, "O3"), ff(150, "4"), ff(39, "4"),
		ff(14, "0"), ff(151, "0"), ff(6, "0"))
	if err := sim.ApplyRemoteExecutionReport(rec); err != nil {
		t.Fatalf("cancel confirm apply failed: %v", err)
	}
	o, _ = store.Get("O3")
	if o.Status != orders.StatusCanceled {
		t.Errorf("want CANCELED, got %s", o.Status)
	}
}

func TestApplyRemoteReplaceConfirm(t *testing.T) {
	sim, _, store := newTestSim(t)
	outboundOrder(t, store, "O4")
	o, _ := store.Get("O4")
	o.PendingReplace = &orders.PendingReplace{ClOrdID: "R9", OrderQty: 200, Price: 55}
	if err := store.Update(o); err != nil {
		t.Fatal(err)
	}
	rec := remoteRec("8",
		ff(11, "R9"), ff(41, "O4"), ff(150, "5"), ff(39, "5"),
		ff(14, "0"), ff(151, "200"), ff(6, "55"))
	if err := sim.ApplyRemoteExecutionReport(rec); err != nil {
		t.Fatalf("replace confirm apply failed: %v", err)
	}
	o, _ = store.Get("O4")
	if o.Status != orders.StatusReplaced || o.OrderQty != 200 || o.Price != 55 {
		t.Errorf("unexpected replaced state: %+v", o)
	}
	if o.PendingReplace != nil {
		t.Errorf("pending replace should be cleared")
	}
}

func TestApplyRemoteCancelReject(t *testing.T) {
	sim, _, store := newTestSim(t)
	outboundOrder(t, store, "O5")
	o, _ := store.Get("O5")
	o.Status = orders.StatusPendingCancel
	o.PrevStatus = orders.StatusPartiallyFilled
	o.CumQty = 40
	o.CancelReqClOrdID = "C9"
	if err := store.Update(o); err != nil {
		t.Fatal(err)
	}
	rec := remoteRec("9",
		ff(11, "C9"), ff(41, "O5"), ff(39, "0"), ff(434, "1"))
	if err := sim.ApplyRemoteExecutionReport(rec); err != nil {
		t.Fatalf("cancel reject apply failed: %v", err)
	}
	o, _ = store.Get("O5")
	if o.Status != orders.StatusPartiallyFilled || o.CancelReqClOrdID != "" {
		t.Errorf("want restored PARTIALLY_FILLED, got %+v", o)
	}
}

func TestApplyRemoteUnknownOrder(t *testing.T) {
	sim, _, _ := newTestSim(t)
	rec := remoteRec("8", ff(11, "GHOST"), ff(150, "F"), ff(39, "2"))
	if err := sim.ApplyRemoteExecutionReport(rec); err == nil {
		t.Errorf("expected error for unknown order")
	}
}

func TestApplyRemoteRejectsInboundOrder(t *testing.T) {
	sim, _, store := newTestSim(t)
	// An INBOUND (acceptor-side) order must never be driven by remote ERs.
	if err := store.Add(&orders.Order{
		OrderID: "FLB9", ClOrdID: "IN1", Direction: orders.DirectionInbound,
		Status: orders.StatusNew,
	}); err != nil {
		t.Fatal(err)
	}
	rec := remoteRec("8", ff(11, "IN1"), ff(150, "F"), ff(39, "2"))
	if err := sim.ApplyRemoteExecutionReport(rec); err == nil {
		t.Errorf("expected rejection for non-OUTBOUND order")
	}
}
