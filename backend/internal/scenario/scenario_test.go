package scenario

import (
	"testing"
	"time"
)

func TestRecorderOrderingAndOffsets(t *testing.T) {
	r := NewRecorder(time.Now())
	r.Record(KindLogon, map[string]any{})
	time.Sleep(5 * time.Millisecond)
	r.Record(KindInbound, InboundPayload{MsgType: "D", Fields: map[int]string{11: "A1"}})
	time.Sleep(5 * time.Millisecond)
	r.Record(KindExecution, ExecutionPayload{Action: ActionFill, ClOrdID: "A1", Qty: 100, Price: 10})

	snap := r.Snapshot()
	if len(snap.Steps) != 3 {
		t.Fatalf("want 3 steps, got %d", len(snap.Steps))
	}
	for i, want := range []string{KindLogon, KindInbound, KindExecution} {
		if snap.Steps[i].Kind != want {
			t.Fatalf("step %d: want kind %q, got %q", i, want, snap.Steps[i].Kind)
		}
		if snap.Steps[i].Seq != i+1 {
			t.Fatalf("step %d: want seq %d, got %d", i, i+1, snap.Steps[i].Seq)
		}
	}
	if !(snap.Steps[0].AtMs <= snap.Steps[1].AtMs && snap.Steps[1].AtMs <= snap.Steps[2].AtMs) {
		t.Fatalf("atMs not non-decreasing: %v", snap.Steps)
	}
	// Inbound payload round-trips through the JSON-shaped map.
	p, err := DecodePayload[InboundPayload](snap.Steps[1].Payload)
	if err != nil {
		t.Fatalf("decode inbound: %v", err)
	}
	if p.MsgType != "D" || p.Fields[11] != "A1" {
		t.Fatalf("inbound payload mismatch: %+v", p)
	}
}

func TestRecorderCapDropsOldest(t *testing.T) {
	r := NewRecorder(time.Now())
	for i := 0; i < MaxSteps+50; i++ {
		r.Record(KindInbound, map[string]any{"i": i})
	}
	if got := r.Len(); got != MaxSteps {
		t.Fatalf("want %d steps after overflow, got %d", MaxSteps, got)
	}
	snap := r.Snapshot()
	if snap.Dropped != 50 {
		t.Fatalf("want 50 dropped, got %d", snap.Dropped)
	}
	// The oldest surviving step is #51 (seq is never reused).
	if snap.Steps[0].Seq != 51 {
		t.Fatalf("want oldest surviving seq 51, got %d", snap.Steps[0].Seq)
	}
	if snap.Steps[len(snap.Steps)-1].Seq != MaxSteps+50 {
		t.Fatalf("want newest seq %d, got %d", MaxSteps+50, snap.Steps[len(snap.Steps)-1].Seq)
	}
}

func TestRecorderPaused(t *testing.T) {
	r := NewRecorder(time.Now())
	r.SetPaused(true)
	r.Record(KindLogon, map[string]any{})
	if r.Len() != 0 {
		t.Fatalf("paused recorder must not record, got %d steps", r.Len())
	}
	r.SetPaused(false)
	r.Record(KindLogon, map[string]any{})
	if r.Len() != 1 {
		t.Fatalf("unpaused recorder must record, got %d steps", r.Len())
	}
}

func TestValidate(t *testing.T) {
	mkStep := func(kind string, payload any) Step {
		return Step{Seq: 1, AtMs: 0, Kind: kind, Payload: PayloadMap(payload)}
	}
	good := &Scenario{
		Role: "ACCEPTOR",
		Steps: []Step{
			mkStep(KindLogon, map[string]any{}),
			mkStep(KindInbound, InboundPayload{MsgType: "D", Fields: map[int]string{11: "A"}}),
			mkStep(KindExecution, ExecutionPayload{Action: ActionFill, ClOrdID: "A", Qty: 1, Price: 1}),
			mkStep(KindLogout, map[string]any{}),
		},
	}
	if err := Validate(good); err != nil {
		t.Fatalf("valid scenario rejected: %v", err)
	}

	cases := []struct {
		name string
		sc   *Scenario
	}{
		{"nil", nil},
		{"empty", &Scenario{Steps: []Step{}}},
		{"unknown kind", &Scenario{Steps: []Step{mkStep("bogus", map[string]any{})}}},
		{"initiator role", &Scenario{Role: "INITIATOR", Steps: []Step{mkStep(KindLogon, map[string]any{})}}},
		{"bad inbound msgtype", &Scenario{Steps: []Step{mkStep(KindInbound, InboundPayload{MsgType: "8"})}}},
		{"unknown action", &Scenario{Steps: []Step{mkStep(KindExecution, ExecutionPayload{Action: "ZAP", ClOrdID: "A"})}}},
		{"missing clOrdId", &Scenario{Steps: []Step{mkStep(KindExecution, ExecutionPayload{Action: ActionFill})}}},
	}
	for _, c := range cases {
		if err := Validate(c.sc); err == nil {
			t.Fatalf("%s: want validation error, got nil", c.name)
		}
	}
}

func TestValidateSortsByAtMs(t *testing.T) {
	mk := func(atMs int64) Step {
		return Step{Seq: 1, AtMs: atMs, Kind: KindLogon, Payload: map[string]any{}}
	}
	sc := &Scenario{Steps: []Step{mk(300), mk(100), mk(200)}}
	if err := Validate(sc); err != nil {
		t.Fatalf("validate: %v", err)
	}
	for i, want := range []int64{100, 200, 300} {
		if sc.Steps[i].AtMs != want {
			t.Fatalf("step %d: want atMs %d, got %d", i, want, sc.Steps[i].AtMs)
		}
	}
}

func TestStepDelay(t *testing.T) {
	if d := StepDelay(0, 1000, 1); d != time.Second {
		t.Fatalf("speed 1: want 1s, got %v", d)
	}
	if d := StepDelay(0, 1000, 2); d != 500*time.Millisecond {
		t.Fatalf("speed 2: want 500ms, got %v", d)
	}
	if d := StepDelay(500, 1500, 4); d != 250*time.Millisecond {
		t.Fatalf("speed 4 over 1000ms span: want 250ms, got %v", d)
	}
	if d := StepDelay(0, 60000, 0); d != 0 {
		t.Fatalf("speed 0: want immediate, got %v", d)
	}
	if d := StepDelay(0, 60000, -1); d != 0 {
		t.Fatalf("negative speed: want immediate, got %v", d)
	}
	// Out-of-order steps never sleep backwards.
	if d := StepDelay(2000, 1000, 1); d != 0 {
		t.Fatalf("backwards step: want 0, got %v", d)
	}
}
