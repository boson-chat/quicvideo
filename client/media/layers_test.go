package media

import (
	"testing"

	"github.com/boson-chat/quicvideo/gen/controlpb"
)

func TestCameraLayers(t *testing.T) {
	ls := CameraLayers(1920, 1080, 30)
	want := [][3]uint32{{320, 180, 15}, {640, 360, 30}, {1280, 720, 30}}
	if len(ls) != 3 {
		t.Fatalf("%d layers", len(ls))
	}
	for i, l := range ls {
		if l.GetId() != uint32(i) || l.GetWidth() != want[i][0] || l.GetHeight() != want[i][1] || l.GetFps() != want[i][2] {
			t.Errorf("layer %d = %v, want %v", i, l, want[i])
		}
		if i > 0 && l.GetBitrate() <= ls[i-1].GetBitrate() {
			t.Errorf("bitrates must increase: %v", ls)
		}
	}
	// ~1.2 Mbps at 720p30, like the bots' synthetic ladder.
	if br := ls[2].GetBitrate(); br < 900_000 || br > 1_500_000 {
		t.Errorf("720p30 bitrate %d", br)
	}
	// Portrait 4:3 camera and odd sizes stay even and keep the aspect.
	ls = CameraLayers(721, 961, 24)
	for _, l := range ls {
		if l.GetWidth()%2 != 0 || l.GetHeight()%2 != 0 || l.GetHeight() > 720 {
			t.Errorf("bad layer %v", l)
		}
	}
	if CameraLayers(0, 720, 30) != nil {
		t.Error("zero size")
	}
}

func TestScreenLayers(t *testing.T) {
	ls := ScreenLayers(3024, 1964) // MacBook Pro Retina
	if len(ls) != 2 || ls[0].GetWidth() != 1280 || ls[0].GetFps() != 15 || ls[1].GetWidth() != 1920 || ls[1].GetFps() != ScreenFPS {
		t.Fatalf("%v", ls)
	}
	if ls[1].GetHeight() != 1246 { // 1964 × 1920/3024 = 1247 → even
		t.Errorf("aspect: %v", ls[1])
	}
	if ls[0].GetBitrate() < 400_000 || ls[1].GetBitrate() > 4_000_000 || ls[1].GetBitrate() <= ls[0].GetBitrate() {
		t.Errorf("bitrates %v", ls)
	}
	if one := ScreenLayers(1024, 768); len(one) != 1 || one[0].GetFps() != ScreenFPS {
		t.Errorf("small screen: %v", one)
	}
}

func TestLayerForTile(t *testing.T) {
	cam := []*controlpb.Layer{{Id: 0, Width: 320, Height: 180}, {Id: 1, Width: 640, Height: 360}, {Id: 2, Width: 1280, Height: 720}}
	cases := []struct {
		w, h    int
		visible bool
		want    uint32
	}{
		{300, 170, true, 0},   // thumbnail
		{350, 200, true, 0},   // 9% upscale of 320×180 is fine
		{400, 225, true, 1},   // too much for layer 0
		{1280, 720, true, 2},  // full
		{2560, 1440, true, 2}, // bigger than the top: top
		{640, 1000, true, 1},  // tall tile, width-bound: 640×360 fits exactly
		{640, 360, false, 0},  // hidden: unsubscribe
	}
	for _, c := range cases {
		got, ok := LayerForTile(cam, c.w, c.h, c.visible)
		if ok != c.visible || (ok && got != c.want) {
			t.Errorf("tile %dx%d visible=%v: got %d,%v want %d", c.w, c.h, c.visible, got, ok, c.want)
		}
	}
	// Order of the layer list doesn't matter.
	rev := []*controlpb.Layer{cam[2], cam[0], cam[1]}
	if got, _ := LayerForTile(rev, 300, 170, true); got != 0 {
		t.Errorf("unsorted: %d", got)
	}
}

func TestDefaultCamera(t *testing.T) {
	cams := []Camera{
		{ID: "v", Name: "Detail Camera", Kind: CameraVirtual},
		{ID: "p", Name: "iPhone", Kind: CameraContinuity},
		{ID: "u", Name: "MX Brio", Kind: CameraExternal},
		{ID: "f", Name: "FaceTime HD Camera", Kind: CameraBuiltIn},
	}
	for n, want := range map[int]string{4: "f", 3: "u", 2: "p", 1: "v"} {
		if c, _ := DefaultCamera(cams[:n]); c.ID != want {
			t.Errorf("first %d cameras: got %q, want %q", n, c.ID, want)
		}
	}
	if _, ok := DefaultCamera(nil); ok {
		t.Fatal("no cameras")
	}

	// Lid closed: the built-in is skipped for the next best one, and only
	// picked (to fail with its reason) when it's the only camera.
	closed := append([]Camera(nil), cams...)
	closed[3].Unavailable = LidClosedReason
	for n, want := range map[int]string{4: "u", 3: "u", 2: "p", 1: "v"} {
		if c, _ := DefaultCamera(closed[:n]); c.ID != want {
			t.Errorf("lid closed, first %d cameras: got %q, want %q", n, c.ID, want)
		}
	}
	if c, ok := DefaultCamera(closed[3:]); !ok || c.ID != "f" || c.Unavailable == "" {
		t.Fatalf("only the closed built-in: got %+v", c)
	}
}

func TestScreenLayersFor(t *testing.T) {
	d := ScreenLayersFor(3024, 1964, ScreenDetail) // MacBook Pro Retina
	if len(d) != 2 || d[0].GetFps() != 15 || d[1].GetFps() != 15 || d[1].GetHeight() > 1440 || d[1].GetWidth() <= 1920 {
		t.Fatalf("detail %v", d)
	}
	m := ScreenLayersFor(3024, 1964, ScreenMotion)
	if len(m) != 2 || m[1].GetFps() != ScreenFPS || m[1].GetWidth() != 1920 {
		t.Fatalf("motion %v", m)
	}
	if w, fps := ScreenCapture(ScreenDetail); w != 2560 || fps != 15 {
		t.Fatalf("detail capture %d@%d", w, fps)
	}
	if w, fps := ScreenCapture(""); w != 1920 || fps != ScreenFPS {
		t.Fatalf("default capture %d@%d", w, fps)
	}
}

func TestCameraLayersFor(t *testing.T) {
	dims := func(ls []*controlpb.Layer) [][3]uint32 {
		out := make([][3]uint32, len(ls))
		for i, l := range ls {
			out[i] = [3]uint32{l.GetWidth(), l.GetHeight(), l.GetFps()}
			if l.GetId() != uint32(i) {
				t.Errorf("layer %d has id %d", i, l.GetId())
			}
			if i > 0 && l.GetBitrate() <= ls[i-1].GetBitrate() {
				t.Errorf("bitrates must increase: %v", ls)
			}
		}
		return out
	}
	eq := func(a, b [][3]uint32) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	// Auto is CameraLayers.
	if got, want := dims(CameraLayersFor(1920, 1080, 30, CameraAuto)), dims(CameraLayers(1920, 1080, 30)); !eq(got, want) {
		t.Errorf("auto = %v, want %v", got, want)
	}
	// Data saver: 360p at the top.
	if got := dims(CameraLayersFor(1280, 720, 30, CameraDataSaver)); !eq(got, [][3]uint32{{320, 180, 15}, {640, 360, 30}}) {
		t.Errorf("data saver = %v", got)
	}
	for _, l := range CameraLayersFor(1080, 1920, 30, CameraDataSaver) { // portrait phone
		if l.GetHeight() > 360 || l.GetWidth()%2 != 0 || l.GetHeight()%2 != 0 {
			t.Errorf("portrait data saver layer %v", l)
		}
	}
	// High: a 1080p camera gets a 1080p layer on top, ≤ 2.5 Mbps.
	hi := CameraLayersFor(1920, 1080, 30, CameraHigh)
	if got := dims(hi); !eq(got, [][3]uint32{{320, 180, 15}, {640, 360, 30}, {1280, 720, 30}, {1920, 1080, 30}}) {
		t.Errorf("high = %v", got)
	}
	if br := hi[3].GetBitrate(); br < 2_000_000 || br > 2_500_000 {
		t.Errorf("1080p30 bitrate %d", br)
	}
	// ... and a 720p camera can't do better than Auto.
	if got, want := dims(CameraLayersFor(1280, 720, 30, CameraHigh)), dims(CameraLayers(1280, 720, 30)); !eq(got, want) {
		t.Errorf("high on 720p = %v, want %v", got, want)
	}
	if CameraLayersFor(0, 0, 30, CameraHigh) != nil || CameraLayersFor(640, 480, 0, CameraDataSaver) != nil {
		t.Error("zero size")
	}
}
