package wire

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestMediaRoundTrip(t *testing.T) {
	h := GroupHeader{TrackID: 7, Layer: 2, Group: 1 << 40}
	frames := []*Frame{
		{Keyframe: true, CaptureUS: 1_700_000_000_000_000, Payload: bytes.Repeat([]byte{1}, 5000)},
		{CaptureUS: 1_700_000_000_033_333, Payload: []byte{2, 3}},
		{CaptureUS: 1_700_000_000_066_666, Payload: nil},
	}
	b := AppendGroupHeader(nil, h)
	for _, f := range frames {
		b = AppendFrame(b, f)
	}
	r, err := NewMediaReader(bytes.NewReader(b), 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.Header() != h {
		t.Fatalf("header %+v want %+v", r.Header(), h)
	}
	for i, want := range frames {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if got.Keyframe != want.Keyframe || got.CaptureUS != want.CaptureUS || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("frame %d mismatch", i)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestMediaFirstFrameMustBeKeyframe(t *testing.T) {
	b := AppendGroupHeader(nil, GroupHeader{TrackID: 1})
	b = AppendFrame(b, &Frame{Payload: []byte{1}})
	r, err := NewMediaReader(bytes.NewReader(b), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); !errors.Is(err, ErrNotKeyframe) {
		t.Fatalf("want ErrNotKeyframe, got %v", err)
	}
}

func TestMediaLimits(t *testing.T) {
	b := AppendGroupHeader(nil, GroupHeader{TrackID: 1})
	b = AppendFrame(b, &Frame{Keyframe: true, Payload: make([]byte, 100)})
	r, _ := NewMediaReader(bytes.NewReader(b), 10)
	if _, err := r.Next(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}

	bad := append(AppendGroupHeader(nil, GroupHeader{TrackID: 1}), 0x80)
	r, _ = NewMediaReader(bytes.NewReader(bad), 0)
	if _, err := r.Next(); !errors.Is(err, ErrBadFlags) {
		t.Fatalf("want ErrBadFlags, got %v", err)
	}

	trunc := AppendFrame(AppendGroupHeader(nil, GroupHeader{TrackID: 1}), &Frame{Keyframe: true, Payload: make([]byte, 10)})
	r, _ = NewMediaReader(bytes.NewReader(trunc[:len(trunc)-3]), 0)
	if _, err := r.Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want ErrUnexpectedEOF, got %v", err)
	}

	if _, err := NewMediaReader(bytes.NewReader([]byte{0x05}), 0); !errors.Is(err, ErrBadGroupHeader) {
		t.Fatalf("want ErrBadGroupHeader, got %v", err)
	}
}
