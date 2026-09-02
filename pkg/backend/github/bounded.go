package github

// boundedWriter is an io.Writer that keeps the first n bytes written to it
// and silently discards the rest. Write always returns len(p), nil, so a
// child process writing to it is never blocked or errored — the child's
// lifetime is governed by its context timeout, not by the reader stopping.
type boundedWriter struct {
	buf []byte
	max int
	n   int // total bytes offered, including discarded
}

func newBoundedWriter(n int) *boundedWriter {
	return &boundedWriter{buf: make([]byte, 0, n), max: n}
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if keep := w.max - len(w.buf); keep > 0 {
		if keep > len(p) {
			keep = len(p)
		}
		w.buf = append(w.buf, p[:keep]...)
	}
	w.n += len(p)
	return len(p), nil
}

// Bytes returns the retained prefix.
func (w *boundedWriter) Bytes() []byte { return w.buf }

// Overflowed reports whether more than the bound was written.
func (w *boundedWriter) Overflowed() bool { return w.n > w.max }
