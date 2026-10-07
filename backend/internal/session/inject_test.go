package session

import (
	"strings"
	"testing"

	"fixlab.dev/fixlab/backend/internal/orders"
)

func testInitiatorSession(role Role, st Status) *Session {
	return &Session{
		Role:       role,
		initStatus: st,
		orderStore: orders.NewInMemoryStore(10),
	}
}

func injectStatus(t *testing.T, s *Session, req InjectRequest) int {
	t.Helper()
	_, err := s.InjectOrder(req)
	if err == nil {
		t.Fatal("expected error, got success")
	}
	ie, ok := err.(*InjectError)
	if !ok {
		t.Fatalf("expected *InjectError, got %T: %v", err, err)
	}
	return ie.Status
}

func TestInjectOrderValidation(t *testing.T) {
	dFields := map[string]string{"11": "A1", "55": "T", "54": "1", "38": "10", "40": "2"}

	t.Run("acceptor role rejected", func(t *testing.T) {
		s := testInitiatorSession(RoleAcceptor, StatusConnected)
		if st := injectStatus(t, s, InjectRequest{MsgType: "D", Fields: dFields}); st != 400 {
			t.Errorf("want 400, got %d", st)
		}
	})
	t.Run("unknown msgType", func(t *testing.T) {
		s := testInitiatorSession(RoleInitiator, StatusLogonAccepted)
		if st := injectStatus(t, s, InjectRequest{MsgType: "X", Fields: dFields}); st != 400 {
			t.Errorf("want 400, got %d", st)
		}
	})
	t.Run("not connected", func(t *testing.T) {
		for _, st := range []Status{StatusConnecting, StatusTCPConnected, StatusLogonSent, StatusConnectionFailed} {
			s := testInitiatorSession(RoleInitiator, st)
			if code := injectStatus(t, s, InjectRequest{MsgType: "D", Fields: dFields}); code != 400 {
				t.Errorf("status %s: want 400, got %d", st, code)
			}
		}
	})
	t.Run("missing required field", func(t *testing.T) {
		s := testInitiatorSession(RoleInitiator, StatusLogonAccepted)
		f := map[string]string{"11": "A1", "55": "T", "54": "1", "38": "10"} // no 40
		if code := injectStatus(t, s, InjectRequest{MsgType: "D", Fields: f}); code != 400 {
			t.Errorf("want 400, got %d", code)
		}
	})
	t.Run("engine-stamped tag rejected", func(t *testing.T) {
		s := testInitiatorSession(RoleInitiator, StatusLogonAccepted)
		f := map[string]string{"11": "A1", "55": "T", "54": "1", "38": "10", "40": "2", "49": "EVIL"}
		_, err := s.InjectOrder(InjectRequest{MsgType: "D", Fields: f})
		if err == nil || !strings.Contains(err.Error(), "stamped by the FIX engine") {
			t.Errorf("want engine-stamped rejection, got: %v", err)
		}
	})
	t.Run("non-numeric tag rejected", func(t *testing.T) {
		s := testInitiatorSession(RoleInitiator, StatusLogonAccepted)
		f := map[string]string{"11": "A1", "55": "T", "54": "1", "38": "10", "40": "2", "xx": "1"}
		if code := injectStatus(t, s, InjectRequest{MsgType: "D", Fields: f}); code != 400 {
			t.Errorf("want 400, got %d", code)
		}
	})
	t.Run("cancel requires known working order", func(t *testing.T) {
		s := testInitiatorSession(RoleInitiator, StatusLogonAccepted)
		f := map[string]string{"11": "C1", "41": "NOPE", "55": "T", "54": "1", "38": "10"}
		if code := injectStatus(t, s, InjectRequest{MsgType: "F", Fields: f}); code != 404 {
			t.Errorf("want 404, got %d", code)
		}
	})
}
