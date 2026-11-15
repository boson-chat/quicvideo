// Package metrics defines quicvideod's OpenTelemetry instruments and the
// meter provider that exports them (Prometheus on /metrics, optional OTLP).
//
// Everything on the media path only bumps plain atomics; the values are
// reported through observable instruments when a reader collects, so a frame
// costs a few atomic adds and never touches the SDK.
package metrics

import (
	"context"
	"fmt"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// ScopeName is the instrumentation scope of every quicvideo instrument.
const ScopeName = "github.com/boson-chat/quicvideo/server"

// DropReason is why the relay discarded media.
type DropReason uint8

const (
	DropSuperseded   DropReason = iota // a newer group started while old frames were queued
	DropOverflow                       // subscriber backlog over SUB_QUEUE_BYTES
	DropStale                          // publisher sent a group older than the current one
	DropRateLimit                      // publisher over its declared bitrate
	DropNoSubscriber                   // subscriber not ready (waiting for a keyframe)
	DropWriteError                     // stream write failed (peer reset, connection gone)
	numDropReasons
)

var dropNames = [numDropReasons]string{"superseded", "overflow", "stale", "rate_limit", "waiting_keyframe", "write_error"}

func (r DropReason) String() string { return dropNames[r] }

// HandshakeOutcome is how a new connection's Hello/Welcome exchange ended.
type HandshakeOutcome uint8

const (
	HandshakeOK HandshakeOutcome = iota
	HandshakeNoStream
	HandshakeNoHello
	HandshakeProtocol
	HandshakeBadToken
	HandshakeExpired
	numHandshakeOutcomes
)

var handshakeNames = [numHandshakeOutcomes]string{"ok", "no_stream", "no_hello", "protocol", "bad_token", "expired"}

func (o HandshakeOutcome) String() string { return handshakeNames[o] }

// TrackKind mirrors controlpb.TrackKind for labels (camera/screen).
type TrackKind uint8

const (
	KindCamera TrackKind = iota
	KindScreen
	numKinds
)

var kindNames = [numKinds]string{"camera", "screen"}

// Metrics holds the counters. The zero value is not usable; use New or Nop.
type Metrics struct {
	connections   atomic.Int64
	sessions      atomic.Int64
	rooms         atomic.Int64
	tracks        [numKinds]atomic.Int64
	subscriptions atomic.Int64

	handshakes [numHandshakeOutcomes]atomic.Int64

	ingressBytes  atomic.Int64
	ingressFrames atomic.Int64
	ingressGroups atomic.Int64
	egressBytes   atomic.Int64
	egressFrames  atomic.Int64
	egressGroups  atomic.Int64

	droppedFrames [numDropReasons]atomic.Int64
	droppedBytes  [numDropReasons]atomic.Int64
	droppedGroups [numDropReasons]atomic.Int64

	keyframeRequests atomic.Int64
	layerSwitches    atomic.Int64
}

// Nop returns metrics that are counted but never exported (tests).
func Nop() *Metrics {
	m, _ := New(noop.NewMeterProvider())
	return m
}

// New registers the observable instruments on mp.
func New(mp metric.MeterProvider) (*Metrics, error) {
	m := &Metrics{}
	meter := mp.Meter(ScopeName)

	gauge := func(name, desc string, cb func(metric.Int64Observer)) error {
		_, err := meter.Int64ObservableUpDownCounter(name, metric.WithDescription(desc),
			metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error { cb(o); return nil }))
		return err
	}
	counter := func(name, unit, desc string, cb func(metric.Int64Observer)) error {
		_, err := meter.Int64ObservableCounter(name, metric.WithUnit(unit), metric.WithDescription(desc),
			metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error { cb(o); return nil }))
		return err
	}
	one := func(v *atomic.Int64) func(metric.Int64Observer) {
		return func(o metric.Int64Observer) { o.Observe(v.Load()) }
	}

	kindAttrs := [numKinds]metric.ObserveOption{}
	for k := range kindAttrs {
		kindAttrs[k] = metric.WithAttributeSet(attribute.NewSet(attribute.String("kind", kindNames[k])))
	}
	dropAttrs := [numDropReasons]metric.ObserveOption{}
	for r := range dropAttrs {
		dropAttrs[r] = metric.WithAttributeSet(attribute.NewSet(attribute.String("reason", dropNames[r])))
	}
	hsAttrs := [numHandshakeOutcomes]metric.ObserveOption{}
	for o := range hsAttrs {
		hsAttrs[o] = metric.WithAttributeSet(attribute.NewSet(attribute.String("outcome", handshakeNames[o])))
	}
	byDrop := func(arr *[numDropReasons]atomic.Int64) func(metric.Int64Observer) {
		return func(o metric.Int64Observer) {
			for r := range arr {
				o.Observe(arr[r].Load(), dropAttrs[r])
			}
		}
	}

	regs := []error{
		gauge("quicvideo.connections", "Open QUIC connections", one(&m.connections)),
		gauge("quicvideo.sessions", "Authenticated sessions", one(&m.sessions)),
		gauge("quicvideo.rooms", "Rooms with at least one participant", one(&m.rooms)),
		gauge("quicvideo.tracks", "Published tracks", func(o metric.Int64Observer) {
			for k := range m.tracks {
				o.Observe(m.tracks[k].Load(), kindAttrs[k])
			}
		}),
		gauge("quicvideo.subscriptions", "Active subscriptions", one(&m.subscriptions)),
		counter("quicvideo.handshakes", "{handshake}", "Handshakes by outcome", func(o metric.Int64Observer) {
			for i := range m.handshakes {
				o.Observe(m.handshakes[i].Load(), hsAttrs[i])
			}
		}),
		counter("quicvideo.ingress.bytes", "By", "Media payload bytes received from publishers", one(&m.ingressBytes)),
		counter("quicvideo.ingress.frames", "{frame}", "Frames received from publishers", one(&m.ingressFrames)),
		counter("quicvideo.ingress.groups", "{group}", "Groups received from publishers", one(&m.ingressGroups)),
		counter("quicvideo.egress.bytes", "By", "Media payload bytes sent to subscribers", one(&m.egressBytes)),
		counter("quicvideo.egress.frames", "{frame}", "Frames sent to subscribers", one(&m.egressFrames)),
		counter("quicvideo.egress.groups", "{group}", "Group streams opened to subscribers", one(&m.egressGroups)),
		counter("quicvideo.dropped.frames", "{frame}", "Frames dropped by reason", byDrop(&m.droppedFrames)),
		counter("quicvideo.dropped.bytes", "By", "Payload bytes dropped by reason", byDrop(&m.droppedBytes)),
		counter("quicvideo.dropped.groups", "{group}", "Groups cut short by reason", byDrop(&m.droppedGroups)),
		counter("quicvideo.keyframe_requests", "{request}", "Keyframe requests sent to publishers", one(&m.keyframeRequests)),
		counter("quicvideo.layer_switches", "{switch}", "Subscriber layer switches", one(&m.layerSwitches)),
	}
	for _, err := range regs {
		if err != nil {
			return nil, fmt.Errorf("metrics: %w", err)
		}
	}
	return m, nil
}

func kindIdx(screen bool) TrackKind {
	if screen {
		return KindScreen
	}
	return KindCamera
}

func (m *Metrics) ConnectionOpened()            { m.connections.Add(1) }
func (m *Metrics) ConnectionClosed()            { m.connections.Add(-1) }
func (m *Metrics) SessionOpened()               { m.sessions.Add(1) }
func (m *Metrics) SessionClosed()               { m.sessions.Add(-1) }
func (m *Metrics) RoomOpened()                  { m.rooms.Add(1) }
func (m *Metrics) RoomClosed()                  { m.rooms.Add(-1) }
func (m *Metrics) TrackAdded(screen bool)       { m.tracks[kindIdx(screen)].Add(1) }
func (m *Metrics) TrackRemoved(screen bool)     { m.tracks[kindIdx(screen)].Add(-1) }
func (m *Metrics) SubscriptionAdded()           { m.subscriptions.Add(1) }
func (m *Metrics) SubscriptionRemoved()         { m.subscriptions.Add(-1) }
func (m *Metrics) Handshake(o HandshakeOutcome) { m.handshakes[o].Add(1) }
func (m *Metrics) IngressGroup()                { m.ingressGroups.Add(1) }
func (m *Metrics) EgressGroup()                 { m.egressGroups.Add(1) }
func (m *Metrics) KeyframeRequested()           { m.keyframeRequests.Add(1) }
func (m *Metrics) LayerSwitched()               { m.layerSwitches.Add(1) }
func (m *Metrics) GroupDropped(r DropReason)    { m.droppedGroups[r].Add(1) }
func (m *Metrics) Ingress(bytes int)            { m.ingressFrames.Add(1); m.ingressBytes.Add(int64(bytes)) }
func (m *Metrics) Egress(bytes int)             { m.egressFrames.Add(1); m.egressBytes.Add(int64(bytes)) }
func (m *Metrics) FrameDropped(r DropReason, bytes int) {
	m.droppedFrames[r].Add(1)
	m.droppedBytes[r].Add(int64(bytes))
}

// Snapshot is a point-in-time copy of the counters (tests, load test).
type Snapshot struct {
	Sessions, Rooms, Tracks, Subscriptions  int64
	IngressBytes, IngressFrames             int64
	EgressBytes, EgressFrames, EgressGroups int64
	DroppedFrames, DroppedGroups            map[string]int64
	KeyframeRequests, LayerSwitches         int64
}

func (m *Metrics) Snapshot() Snapshot {
	s := Snapshot{
		Sessions: m.sessions.Load(), Rooms: m.rooms.Load(), Subscriptions: m.subscriptions.Load(),
		Tracks:       m.tracks[0].Load() + m.tracks[1].Load(),
		IngressBytes: m.ingressBytes.Load(), IngressFrames: m.ingressFrames.Load(),
		EgressBytes: m.egressBytes.Load(), EgressFrames: m.egressFrames.Load(), EgressGroups: m.egressGroups.Load(),
		DroppedFrames: map[string]int64{}, DroppedGroups: map[string]int64{},
		KeyframeRequests: m.keyframeRequests.Load(), LayerSwitches: m.layerSwitches.Load(),
	}
	for r := range m.droppedFrames {
		if v := m.droppedFrames[r].Load(); v > 0 {
			s.DroppedFrames[dropNames[r]] = v
		}
		if v := m.droppedGroups[r].Load(); v > 0 {
			s.DroppedGroups[dropNames[r]] = v
		}
	}
	return s
}
