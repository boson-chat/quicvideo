package controlpb

import "testing"

// The proto file registers under a path unique to quicvideo: a plain
// "control.proto" collides with quicvoice's in an app that links both (the
// protobuf registry panics at startup).
func TestUniqueProtoPath(t *testing.T) {
	if p := File_quicvideo_v1_control_proto.Path(); p != "quicvideo/v1/control.proto" {
		t.Fatalf("registered as %q", p)
	}
}
