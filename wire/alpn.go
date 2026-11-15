// Package wire is the quicvideo wire format: control message framing on the
// control stream and the media stream layout (one unidirectional stream per
// group of pictures).
package wire

// ALPN is the TLS application protocol for quicvideo over QUIC.
const ALPN = "boson-video/1"
