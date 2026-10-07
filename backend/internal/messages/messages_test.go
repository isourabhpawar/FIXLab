package messages

import (
	"testing"
)

func TestIsAdminMsgType(t *testing.T) {
	for _, mt := range []string{"A", "0", "1", "2", "4", "5"} {
		if !IsAdminMsgType(mt) {
			t.Fatalf("msgtype %q should be admin", mt)
		}
	}
	for _, mt := range []string{"D", "8", "F", "G", "9"} {
		if IsAdminMsgType(mt) {
			t.Fatalf("msgtype %q should NOT be admin", mt)
		}
	}
}

func TestRingBuffer_Bounded(t *testing.T) {
	r := NewRingBuffer(3)
	for i := 0; i < 5; i++ {
		r.Push(&Record{MsgSeqNum: i + 1})
	}
	if r.Len() != 3 {
		t.Fatalf("Len = %d, want 3", r.Len())
	}
	list := r.List(0)
	if len(list) != 3 || list[0].MsgSeqNum != 3 || list[2].MsgSeqNum != 5 {
		t.Fatalf("expected seqnums [3 4 5], got %v", seqs(list))
	}
}

func TestRingBuffer_Limit(t *testing.T) {
	r := NewRingBuffer(10)
	for i := 0; i < 5; i++ {
		r.Push(&Record{MsgSeqNum: i + 1})
	}
	list := r.List(2)
	if len(list) != 2 || list[0].MsgSeqNum != 4 {
		t.Fatalf("expected last 2 records, got %v", seqs(list))
	}
}

func TestRingBuffer_Clear(t *testing.T) {
	r := NewRingBuffer(10)
	r.Push(&Record{})
	r.Clear()
	if r.Len() != 0 {
		t.Fatal("Clear did not empty the buffer")
	}
}

func seqs(rs []*Record) []int {
	out := make([]int, len(rs))
	for i, r := range rs {
		out[i] = r.MsgSeqNum
	}
	return out
}
