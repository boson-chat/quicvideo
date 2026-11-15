package media

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/boson-chat/quicvideo/client"
	"github.com/boson-chat/quicvideo/gen/controlpb"
)

// EncoderConfig is one layer's encoder setup.
type EncoderConfig struct {
	Width, Height int
	FPS           int
	Bitrate       uint64        // bits/s
	GOP           time.Duration // max keyframe interval
	Screen        bool          // content hint: text-heavy, mostly static
}

// EncodedFrame is one Annex-B access unit from an encoder.
type EncodedFrame struct {
	Data      []byte
	Keyframe  bool
	CaptureUS uint64
}

// Encoder encodes pictures for one layer. Output arrives on the callback
// given to the factory, possibly on another goroutine, in order.
type Encoder interface {
	// Encode submits a picture (scaled to the layer size by the encoder).
	// The picture may be released by the caller once Encode returns.
	Encode(p Picture, captureUS uint64, forceKeyframe bool) error
	// Close flushes and stops the encoder; no output after it returns.
	Close()
}

// EncoderFactory makes an encoder (VideoToolbox on macOS).
type EncoderFactory func(cfg EncoderConfig, out func(EncodedFrame)) (Encoder, error)

// TrackWriter is what the Publisher needs from a published track
// (*client.LocalTrack).
type TrackWriter interface {
	WriteFrame(layer uint32, keyframe bool, captureUS uint64, payload []byte) error
}

// PublisherOptions tune a Publisher.
type PublisherOptions struct {
	GOP    time.Duration // default 2s camera, 4s screen
	Screen bool
	// OnError gets encoder creation/encode errors (default: ignored).
	OnError func(layer uint32, err error)
	// Now is the clock (tests).
	Now func() time.Time
}

// Publisher feeds captured pictures to one encoder per *watched* layer and
// writes their output to the track. Layers nobody watches have no encoder
// at all (LayerActive), so they cost neither CPU nor upload. Keyframes are
// forced when a layer starts, when the server or the send queue asks, and
// after a frame the track refused for want of one.
type Publisher struct {
	track   TrackWriter
	layers  map[uint32]*controlpb.Layer
	factory EncoderFactory
	opts    PublisherOptions

	mu     sync.Mutex
	encs   map[uint32]*layerEnc
	last   Picture // most recent capture (retained), for keyframes on demand
	lastUS uint64
	closed bool
}

type layerEnc struct {
	id     uint32
	cfg    EncoderConfig
	enc    Encoder
	encMu  sync.Mutex // serializes Encode calls
	force  bool       // next encode is a keyframe (guarded by Publisher.mu)
	lastUS uint64     // last submitted capture time (fps cap)
}

// NewPublisher makes a publisher for layers. Wire SetActive and
// KeyframeNeeded to the track's PublishOptions (PublishTrack does this).
func NewPublisher(track TrackWriter, layers []*controlpb.Layer, factory EncoderFactory, opts PublisherOptions) *Publisher {
	if opts.GOP <= 0 {
		opts.GOP = 2 * time.Second
		if opts.Screen {
			opts.GOP = 4 * time.Second
		}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	p := &Publisher{track: track, layers: map[uint32]*controlpb.Layer{}, factory: factory, opts: opts, encs: map[uint32]*layerEnc{}}
	for _, l := range layers {
		p.layers[l.GetId()] = l
	}
	return p
}

// PublishTrack publishes a track on c for layers and returns the Publisher
// that drives it. Feed it with Push (e.g. from a capture Source).
func PublishTrack(ctx context.Context, c *client.Client, kind controlpb.TrackKind, layers []*controlpb.Layer,
	factory EncoderFactory, opts PublisherOptions) (*Publisher, *client.LocalTrack, error) {
	opts.Screen = kind == controlpb.TrackKind_SCREEN
	p := NewPublisher(nil, layers, factory, opts)
	ready := make(chan struct{})
	track, err := c.Publish(ctx, kind, "h264", layers, client.PublishOptions{
		// Callbacks can arrive before Publish returns: wait for the track.
		OnLayerActive:    func(l uint32, active bool) { <-ready; p.SetActive(l, active) },
		OnKeyframeNeeded: func(l uint32) { <-ready; p.KeyframeNeeded(l) },
	})
	if err != nil {
		close(ready)
		return nil, nil, err
	}
	p.mu.Lock()
	p.track = track
	p.mu.Unlock()
	close(ready)
	return p, track, nil
}

func (p *Publisher) config(l *controlpb.Layer) EncoderConfig {
	return EncoderConfig{Width: int(l.GetWidth()), Height: int(l.GetHeight()), FPS: int(l.GetFps()),
		Bitrate: l.GetBitrate(), GOP: p.opts.GOP, Screen: p.opts.Screen}
}

// SetActive starts or stops a layer's encoder (LayerActive).
func (p *Publisher) SetActive(layer uint32, active bool) {
	l := p.layers[layer]
	if l == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	le := p.encs[layer]
	if !active {
		delete(p.encs, layer)
		p.mu.Unlock()
		if le != nil {
			le.encMu.Lock()
			le.enc.Close()
			le.encMu.Unlock()
		}
		return
	}
	if le != nil {
		le.force = true
		p.mu.Unlock()
		p.kick(layer)
		return
	}
	p.mu.Unlock()

	le = &layerEnc{id: layer, cfg: p.config(l), force: true}
	enc, err := p.factory(le.cfg, func(f EncodedFrame) { p.output(layer, f) })
	if err != nil {
		p.fail(layer, err)
		return
	}
	le.enc = enc
	p.mu.Lock()
	if p.closed || p.encs[layer] != nil {
		p.mu.Unlock()
		enc.Close()
		return
	}
	p.encs[layer] = le
	p.mu.Unlock()
	p.kick(layer)
}

// KeyframeNeeded makes the layer's next frame a keyframe, re-encoding the
// last picture right away (a static screen may not produce another one).
func (p *Publisher) KeyframeNeeded(layer uint32) {
	p.mu.Lock()
	le := p.encs[layer]
	if le != nil {
		le.force = true
	}
	p.mu.Unlock()
	if le != nil {
		p.kick(layer)
	}
}

// kick encodes the last captured picture on one layer if it's waiting for
// a keyframe.
func (p *Publisher) kick(layer uint32) {
	p.mu.Lock()
	le := p.encs[layer]
	pic := p.last
	if le == nil || pic == nil || !le.force {
		p.mu.Unlock()
		return
	}
	pic.Retain()
	le.force = false
	// It's a new frame of what's on screen now (a static screen sends no
	// captures), so it's stamped now: an old capture time would make the
	// layer look seconds late to the relay. Strictly increasing for the
	// encoder.
	us := max(uint64(p.opts.Now().UnixMicro()), p.lastUS+1)
	p.lastUS = us
	le.lastUS = us
	p.mu.Unlock()
	p.encode(le, pic, us, true)
	pic.Release()
}

// Push hands a captured picture to every active layer, honouring each
// layer's fps cap. The picture is only borrowed for the call.
func (p *Publisher) Push(f CapturedFrame) {
	type job struct {
		le    *layerEnc
		force bool
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	f.Picture.Retain()
	if p.last != nil {
		p.last.Release()
	}
	p.last, p.lastUS = f.Picture, f.CaptureUS
	var jobs []job
	for _, le := range p.encs {
		interval := uint64(time.Second.Microseconds()) / uint64(max(le.cfg.FPS, 1))
		// 15% slack so a 30 fps capture isn't thinned to 15 on a 30 fps layer.
		if f.CaptureUS <= le.lastUS {
			continue // not newer than what this encoder already has
		}
		if !le.force && le.lastUS != 0 && f.CaptureUS < le.lastUS+interval*85/100 {
			continue
		}
		jobs = append(jobs, job{le, le.force})
		le.force = false
		le.lastUS = f.CaptureUS
	}
	f.Picture.Retain() // for the encodes below
	p.mu.Unlock()
	for _, j := range jobs {
		p.encode(j.le, f.Picture, f.CaptureUS, j.force)
	}
	f.Picture.Release()
}

func (p *Publisher) encode(le *layerEnc, pic Picture, us uint64, force bool) {
	le.encMu.Lock()
	err := le.enc.Encode(pic, us, force)
	le.encMu.Unlock()
	if err != nil {
		p.mu.Lock()
		if p.encs[le.id] == le {
			le.force = true
		}
		p.mu.Unlock()
		p.fail(le.id, err)
	}
}

// output writes an encoded frame. If the track wants a keyframe first (or
// its send queue just dropped frames), the next encode is forced.
func (p *Publisher) output(layer uint32, f EncodedFrame) {
	p.mu.Lock()
	track := p.track
	p.mu.Unlock()
	if track == nil {
		return
	}
	err := track.WriteFrame(layer, f.Keyframe, f.CaptureUS, f.Data)
	if errors.Is(err, client.ErrNeedKeyframe) || errors.Is(err, client.ErrBackpressure) {
		p.mu.Lock()
		if le := p.encs[layer]; le != nil {
			le.force = true
		}
		p.mu.Unlock()
	}
}

func (p *Publisher) fail(layer uint32, err error) {
	if p.opts.OnError != nil {
		p.opts.OnError(layer, err)
	}
}

// ActiveLayers lists layers with a running encoder (for UI/stats).
func (p *Publisher) ActiveLayers() []uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]uint32, 0, len(p.encs))
	for id := range p.encs {
		out = append(out, id)
	}
	return out
}

// Close stops every encoder.
func (p *Publisher) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	encs := p.encs
	p.encs = map[uint32]*layerEnc{}
	last := p.last
	p.last = nil
	p.mu.Unlock()
	for _, le := range encs {
		le.encMu.Lock()
		le.enc.Close()
		le.encMu.Unlock()
	}
	if last != nil {
		last.Release()
	}
}
