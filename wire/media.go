package wire

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Media streams
//
// Every group of pictures (a keyframe plus the frames that depend on it) of
// one layer of one track travels on its own unidirectional QUIC stream:
//
//	stream:  GroupHeader Frame*
//	header:  varint track_id, varint layer, varint group_seq
//	frame:   byte flags, varint capture_ts_us, varint len, payload[len]
//
// The first frame of a group must be a keyframe. A stream that is reset
// (RESET_STREAM) ends its group early; receivers drop the partial group and
// wait for the next one.

// MaxFrameSize caps one encoded frame (payload bytes).
const MaxFrameSize = 4 << 20

// Frame flags.
const (
	FlagKeyframe byte = 1 << 0
	knownFlags        = FlagKeyframe
)

// Stream error codes used with CancelWrite / CancelRead on media streams.
const (
	// The group was dropped: the receiver is too far behind, or a newer
	// group superseded it.
	StreamCodeDropped = 0x10
	// The group is stale (older than the current one) or otherwise unwanted.
	StreamCodeStale = 0x11
	// The stream broke the media format.
	StreamCodeProtocol = 0x12
	// The track or subscription went away.
	StreamCodeGone = 0x13
)

var (
	ErrFrameTooLarge  = errors.New("wire: frame too large")
	ErrBadFlags       = errors.New("wire: unknown frame flags")
	ErrNotKeyframe    = errors.New("wire: group does not start with a keyframe")
	ErrBadGroupHeader = errors.New("wire: bad group header")
)

// GroupHeader opens a media stream.
type GroupHeader struct {
	TrackID uint32
	Layer   uint32
	Group   uint64
}

// Frame is one encoded video frame. Payload is opaque to the server (it is
// the codec bitstream, e.g. an H.264 access unit in Annex-B form, or an
// end-to-end encrypted blob).
type Frame struct {
	Keyframe  bool
	CaptureUS uint64 // capture time, µs since the Unix epoch (sender's wall clock)
	Payload   []byte
}

// AppendGroupHeader appends the header bytes to dst.
func AppendGroupHeader(dst []byte, h GroupHeader) []byte {
	dst = binary.AppendUvarint(dst, uint64(h.TrackID))
	dst = binary.AppendUvarint(dst, uint64(h.Layer))
	return binary.AppendUvarint(dst, h.Group)
}

// AppendFrameHeader appends a frame's header (everything before the
// payload) to dst. Writers send the payload right after it.
func AppendFrameHeader(dst []byte, f *Frame) []byte {
	var flags byte
	if f.Keyframe {
		flags |= FlagKeyframe
	}
	dst = append(dst, flags)
	dst = binary.AppendUvarint(dst, f.CaptureUS)
	return binary.AppendUvarint(dst, uint64(len(f.Payload)))
}

// AppendFrame appends a whole frame (header and payload) to dst.
func AppendFrame(dst []byte, f *Frame) []byte {
	return append(AppendFrameHeader(dst, f), f.Payload...)
}

// MediaReader reads one media stream: the header, then frames until EOF.
type MediaReader struct {
	r      *bufio.Reader
	header GroupHeader
	n      int // frames read
	max    int
}

// NewMediaReader reads the group header from r. maxFrame <= 0 means
// MaxFrameSize.
func NewMediaReader(r io.Reader, maxFrame int) (*MediaReader, error) {
	if maxFrame <= 0 || maxFrame > MaxFrameSize {
		maxFrame = MaxFrameSize
	}
	br := bufio.NewReaderSize(r, 16<<10)
	track, err := binary.ReadUvarint(br)
	if err != nil {
		return nil, headerErr(err)
	}
	layer, err := binary.ReadUvarint(br)
	if err != nil {
		return nil, headerErr(err)
	}
	group, err := binary.ReadUvarint(br)
	if err != nil {
		return nil, headerErr(err)
	}
	if track > 0xFFFFFFFF || layer > 0xFFFFFFFF {
		return nil, ErrBadGroupHeader
	}
	return &MediaReader{
		r:      br,
		header: GroupHeader{TrackID: uint32(track), Layer: uint32(layer), Group: group},
		max:    maxFrame,
	}, nil
}

func headerErr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: %v", ErrBadGroupHeader, io.ErrUnexpectedEOF)
	}
	return err
}

func (m *MediaReader) Header() GroupHeader { return m.header }

// Next reads the next frame. It returns io.EOF at a clean end of the group.
// The payload is freshly allocated and owned by the caller.
func (m *MediaReader) Next() (*Frame, error) {
	flags, err := m.r.ReadByte()
	if err != nil {
		return nil, err // io.EOF here is the clean end
	}
	if flags&^knownFlags != 0 {
		return nil, ErrBadFlags
	}
	ts, err := binary.ReadUvarint(m.r)
	if err != nil {
		return nil, unexpected(err)
	}
	n, err := binary.ReadUvarint(m.r)
	if err != nil {
		return nil, unexpected(err)
	}
	if n > uint64(m.max) {
		return nil, ErrFrameTooLarge
	}
	f := &Frame{Keyframe: flags&FlagKeyframe != 0, CaptureUS: ts, Payload: make([]byte, n)}
	if _, err := io.ReadFull(m.r, f.Payload); err != nil {
		return nil, unexpected(err)
	}
	if m.n == 0 && !f.Keyframe {
		return nil, ErrNotKeyframe
	}
	m.n++
	return f, nil
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
