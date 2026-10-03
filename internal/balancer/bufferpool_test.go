package balancer

import "testing"

func TestBufferPool(t *testing.T) {
	p := newBufferPool()
	buf := p.Get()
	if len(buf) != 32<<10 {
		t.Fatalf("len = %d, want %d", len(buf), 32<<10)
	}
	p.Put(buf)
	if got := p.Get(); len(got) != 32<<10 {
		t.Errorf("after Put: len = %d, want %d", len(got), 32<<10)
	}
}
