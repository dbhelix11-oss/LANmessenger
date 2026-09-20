package tunnel

import (
	"net"
	"testing"
)

type sample struct {
	A string `json:"a"`
	B int    `json:"b"`
}

func TestFrame_RoundTrip(t *testing.T) {
	r, w := net.Pipe()
	defer r.Close()
	defer w.Close()

	want := sample{A: "hello", B: 42}
	writeErr := make(chan error, 1)
	go func() { writeErr <- WriteFrame(w, want) }()

	var got sample
	if err := ReadFrame(r, &got); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestWriteFrame_RejectsOversized(t *testing.T) {
	r, w := net.Pipe()
	defer r.Close()
	defer w.Close()

	huge := sample{A: string(make([]byte, maxFrameBytes+1))}
	if err := WriteFrame(w, huge); err == nil {
		t.Fatal("expected WriteFrame to reject an oversized frame")
	}
}
