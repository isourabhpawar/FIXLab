package session

import (
	"fmt"
	"net"
	"sync"
	"testing"
)

func TestPortManager_AcquireRelease(t *testing.T) {
	pm := NewPortManager(44000, 44005)
	p1, err := pm.Acquire("s1")
	if err != nil {
		t.Fatal(err)
	}
	if p1 < 44000 || p1 > 44005 {
		t.Fatalf("port %d outside pool", p1)
	}
	p2, err := pm.Acquire("s2")
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Fatal("same port handed out twice")
	}
	pm.Release(p1)
	p3, err := pm.Acquire("s3")
	if err != nil {
		t.Fatal(err)
	}
	if p3 != p1 {
		t.Fatalf("released port %d not reused, got %d", p1, p3)
	}
}

func TestPortManager_Exhaustion(t *testing.T) {
	pm := NewPortManager(44100, 44101) // two ports
	if _, err := pm.Acquire("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := pm.Acquire("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := pm.Acquire("c"); err == nil {
		t.Fatal("expected exhaustion error")
	}
}

func TestPortManager_SkipsExternallyHeldPort(t *testing.T) {
	// Occupy the first pool port outside the manager: the bind probe is
	// authoritative, so Acquire must skip it.
	ln, err := net.Listen("tcp", "127.0.0.1:44200")
	if err != nil {
		t.Skip("port 44200 unavailable for test setup")
	}
	defer ln.Close()

	pm := NewPortManager(44200, 44201)
	p, err := pm.Acquire("s1")
	if err != nil {
		t.Fatal(err)
	}
	if p == 44200 {
		t.Fatal("Acquire returned a port held by an external process")
	}
}

func TestPortManager_ConcurrentUnique(t *testing.T) {
	pm := NewPortManager(44300, 44319)
	var mu sync.Mutex
	seen := map[int]bool{}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := pm.Acquire(fmt.Sprintf("s%d", i))
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			if seen[p] {
				errs <- fmt.Errorf("duplicate port %d", p)
			}
			seen[p] = true
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(seen) != 20 {
		t.Fatalf("expected 20 unique ports, got %d", len(seen))
	}
}

func TestPortManager_ReleaseUnknown(t *testing.T) {
	pm := NewPortManager(44400, 44401)
	pm.Release(44400) // must not panic
}
