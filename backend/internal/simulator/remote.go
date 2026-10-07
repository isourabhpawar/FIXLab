// Remote counterparty handling (phase 5, initiator mode).
//
// When FixLab acts as the initiator, orders are injected OUTBOUND into
// the remote FIX acceptor (POST /send-order). The remote's
// ExecutionReports (35=8) and OrderCancelRejects (35=9) arrive INBOUND
// and are authoritative for the blotter: this file applies them to the
// OUTBOUND order records the injection created.
//
// Rules, the kill switch, and browser executions never touch OUTBOUND
// orders — their lifecycle is driven entirely by the remote.
package simulator

import (
	"errors"
	"strconv"

	"fixlab.dev/fixlab/backend/internal/messages"
	"fixlab.dev/fixlab/backend/internal/orders"
)

// remoteOrdStatus maps a remote 39=OrdStatus onto the blotter status.
// Values the blotter does not model (3=DoneForDay, 7=Stopped,
// 9=Suspended, …) leave the current status untouched; quantities are
// still updated from the report.
func remoteOrdStatus(v string) (orders.Status, bool) {
	switch v {
	case OrdStatusNew: // "0"
		return orders.StatusNew, true
	case OrdStatusPartiallyFilled: // "1"
		return orders.StatusPartiallyFilled, true
	case OrdStatusFilled: // "2"
		return orders.StatusFilled, true
	case OrdStatusCanceled: // "4"
		return orders.StatusCanceled, true
	case "5": // Replaced
		return orders.StatusReplaced, true
	case "6": // PendingCancel
		return orders.StatusPendingCancel, true
	case OrdStatusRejected: // "8"
		return orders.StatusRejected, true
	}
	return "", false
}

// ApplyRemoteExecutionReport applies one inbound 35=8/35=9 from the
// remote counterparty to the matching OUTBOUND order.
func (s *simulator) ApplyRemoteExecutionReport(rec *messages.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	f := fieldMap(rec.Fields)
	switch rec.MsgType {
	case msgOrderCancelReject:
		return s.applyCancelReject(f, rec)
	case msgExecutionReport:
		return s.applyRemoteER(f, rec)
	default:
		return inputErr("remote report has unexpected MsgType %q (want 8 or 9)", rec.MsgType)
	}
}

// applyCancelReject handles an inbound 35=9: the remote refused our
// cancel (434=1) or replace (434=2) request. Tag 41 identifies the
// order; the request's own ClOrdID is tag 11.
func (s *simulator) applyCancelReject(f map[int]string, rec *messages.Record) error {
	targetID := f[int(tagOrigClOrdID)]
	if targetID == "" {
		targetID = f[int(tagClOrdID)]
	}
	if targetID == "" {
		return inputErr("remote 35=9 missing OrigClOrdID (41)")
	}
	o, err := s.outboundOrder(targetID)
	if err != nil {
		return err
	}
	// Restore the pre-request state: a rejected cancel leaves the order
	// working again; a rejected replace drops the pending request.
	if o.Status == orders.StatusPendingCancel {
		o.Status = o.PrevStatus
		if o.Status == "" {
			o.Status = orders.StatusNew
		}
		o.PrevStatus = ""
		o.CancelReqClOrdID = ""
	}
	o.PendingReplace = nil
	if uerr := s.store.Update(o); uerr != nil {
		return uerr
	}
	s.logger().Info("simulator: remote cancel/replace rejected",
		"cl_ord_id", o.ClOrdID, "status", string(o.Status))
	s.publishRemoteReport(o, rec, f)
	return nil
}

// applyRemoteER handles an inbound 35=8: quantities and status come
// straight from the report (the remote is authoritative).
func (s *simulator) applyRemoteER(f map[int]string, rec *messages.Record) error {
	// Which tag identifies the order? For cancel/replace confirmations
	// (150=4/5) tag 11 is the REQUEST's ClOrdID and tag 41 names the
	// order; for everything else (including order rejects, 150=8) tag 11
	// is the order's own ClOrdID. As a fallback, some venues echo the
	// order in 41 on every report.
	clOrdID := f[int(tagClOrdID)]
	execType := f[int(tagExecType)]
	orig := f[int(tagOrigClOrdID)]
	if (execType == ExecTypeCanceled || execType == ExecTypeReplaced) && orig != "" {
		clOrdID = orig
	}
	if clOrdID == "" {
		return inputErr("remote 35=8 missing ClOrdID (11)")
	}
	o, err := s.outboundOrder(clOrdID)
	if err != nil && errors.Is(err, orders.ErrOrderNotFound) && orig != "" && orig != clOrdID {
		o, err = s.outboundOrder(orig)
	}
	if err != nil {
		return err
	}
	// Quantities: the remote's CumQty/LeavesQty/AvgPx win when present.
	if v := f[int(tagCumQty)]; v != "" {
		if q, perr := strconv.ParseFloat(v, 64); perr == nil {
			o.CumQty = q
		}
	}
	if v := f[int(tagLeavesQty)]; v != "" {
		if q, perr := strconv.ParseFloat(v, 64); perr == nil {
			o.LeavesQty = q
		}
	}
	if v := f[int(tagAvgPx)]; v != "" {
		if p, perr := strconv.ParseFloat(v, 64); perr == nil {
			o.AvgPx = p
		}
	}
	if st, ok := remoteOrdStatus(f[int(tagOrdStatus)]); ok {
		// A confirmed replace applies the requested new terms and
		// consumes the pending request (the order itself stays working).
		if st == orders.StatusReplaced {
			if o.PendingReplace != nil {
				o.OrderQty = o.PendingReplace.OrderQty
				if o.PendingReplace.Price > 0 {
					o.Price = o.PendingReplace.Price
				}
				o.LeavesQty = o.OrderQty - o.CumQty
				if o.LeavesQty < 0 {
					o.LeavesQty = 0
				}
			}
			o.PendingReplace = nil
		}
		o.Status = st
		if st.Terminal() {
			o.PendingReplace = nil
			o.CancelReqClOrdID = ""
		}
	}
	if uerr := s.store.Update(o); uerr != nil {
		return uerr
	}
	s.logger().Info("simulator: remote execution report applied",
		"cl_ord_id", o.ClOrdID,
		"exec_type", f[int(tagExecType)], "ord_status", f[int(tagOrdStatus)],
		"cum_qty", o.CumQty, "leaves_qty", o.LeavesQty)
	s.publishRemoteReport(o, rec, f)
	return nil
}

// outboundOrder loads an order and verifies it is OUTBOUND-injected
// (only those have their lifecycle driven by the remote).
func (s *simulator) outboundOrder(clOrdID string) (*orders.Order, error) {
	o, ok := s.store.Get(clOrdID)
	if !ok {
		return nil, orders.ErrOrderNotFound
	}
	if o.Direction != orders.DirectionOutbound {
		return nil, inputErr("remote report for %s: order is not OUTBOUND-injected", clOrdID)
	}
	return o, nil
}

// publishRemoteReport streams ORDER_UPDATED and EXECUTION_SENT for a
// remote-driven update, mirroring the local send() path.
func (s *simulator) publishRemoteReport(o *orders.Order, rec *messages.Record, f map[int]string) {
	var lastQty, lastPx float64
	if v := f[int(tagLastQty)]; v != "" {
		lastQty, _ = strconv.ParseFloat(v, 64)
	}
	if v := f[int(tagLastPx)]; v != "" {
		lastPx, _ = strconv.ParseFloat(v, 64)
	}
	sum := &ExecutionSummary{
		MsgType:   rec.MsgType,
		ExecType:  f[int(tagExecType)],
		OrdStatus: f[int(tagOrdStatus)],
		ExecID:    f[int(tagExecID)],
		OrderID:   f[int(tagOrderID)],
		ClOrdID:   o.ClOrdID,
		CumQty:    o.CumQty,
		LeavesQty: o.LeavesQty,
		AvgPx:     o.AvgPx,
		LastQty:   lastQty,
		LastPx:    lastPx,
		RawFix:    rec.RawFIX,
	}
	s.sender.PublishOrderEvent("ORDER_UPDATED", o)
	s.sender.PublishExecutionSent(sum)
}
