package github

import (
	"bytes"
	"testing"
)

func TestBoundedWriter_UnderLimit(t *testing.T) {
	w := newBoundedWriter(100)
	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
	if !bytes.Equal(w.Bytes(), []byte("hello")) {
		t.Errorf("Bytes = %q, want %q", w.Bytes(), "hello")
	}
	if w.Overflowed() {
		t.Error("Overflowed should be false when under limit")
	}
}

func TestBoundedWriter_ExactlyAtLimit(t *testing.T) {
	w := newBoundedWriter(5)
	w.Write([]byte("exact"))
	if !bytes.Equal(w.Bytes(), []byte("exact")) {
		t.Errorf("Bytes = %q, want %q", w.Bytes(), "exact")
	}
	if w.Overflowed() {
		t.Error("Overflowed should be false at exactly the limit")
	}
}

func TestBoundedWriter_OverLimit(t *testing.T) {
	w := newBoundedWriter(5)
	n, err := w.Write([]byte("hello world"))
	if err != nil || n != 11 {
		t.Fatalf("Write = (%d, %v), want (11, nil)", n, err)
	}
	if !bytes.Equal(w.Bytes(), []byte("hello")) {
		t.Errorf("Bytes = %q, want first 5 bytes %q", w.Bytes(), "hello")
	}
	if !w.Overflowed() {
		t.Error("Overflowed should be true when over limit")
	}
}

func TestBoundedWriter_MultipleWritesStraddling(t *testing.T) {
	w := newBoundedWriter(8)
	w.Write([]byte("hel"))   // 3, under
	w.Write([]byte("lo wo")) // 5, straddles at 8
	w.Write([]byte("rld"))   // 3, entirely past

	if !bytes.Equal(w.Bytes(), []byte("hello wo")) {
		t.Errorf("Bytes = %q, want %q", w.Bytes(), "hello wo")
	}
	if !w.Overflowed() {
		t.Error("Overflowed should be true")
	}
}

func TestBoundedWriter_ZeroLengthWrites(t *testing.T) {
	w := newBoundedWriter(10)
	n, err := w.Write([]byte{})
	if err != nil || n != 0 {
		t.Fatalf("Write = (%d, %v), want (0, nil)", n, err)
	}
	w.Write([]byte("abc"))
	n, err = w.Write([]byte{})
	if err != nil || n != 0 {
		t.Fatalf("Write after data = (%d, %v), want (0, nil)", n, err)
	}
	if !bytes.Equal(w.Bytes(), []byte("abc")) {
		t.Errorf("Bytes = %q, want %q", w.Bytes(), "abc")
	}
	if w.Overflowed() {
		t.Error("Overflowed should be false")
	}
}

func TestBoundedWriter_AlwaysReportsFullLength(t *testing.T) {
	w := newBoundedWriter(3)
	n, err := w.Write([]byte("abcdef"))
	if n != 6 || err != nil {
		t.Errorf("Write = (%d, %v), want (6, nil) — the child must never see a short write", n, err)
	}
	n, err = w.Write([]byte("ghi"))
	if n != 3 || err != nil {
		t.Errorf("second Write = (%d, %v), want (3, nil)", n, err)
	}
}
