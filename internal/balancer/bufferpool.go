package balancer

import "sync"

// bufferPool is an httputil.BufferPool shared by every proxy. Without one,
// ReverseProxy allocates a new 32KB buffer to copy each response body.
type bufferPool struct {
	pool sync.Pool
}

func newBufferPool() *bufferPool {
	return &bufferPool{pool: sync.Pool{New: func() any {
		buf := make([]byte, 32<<10)
		return &buf
	}}}
}

func (p *bufferPool) Get() []byte {
	return *p.pool.Get().(*[]byte)
}

// Put stores a pointer, as a slice stored directly would be copied into a
// new allocation on every Put.
func (p *bufferPool) Put(buf []byte) {
	p.pool.Put(&buf)
}
