package media

import "testing"

func TestUprightRotation(t *testing.T) {
	cases := []struct {
		sensor, device int
		front          bool
		want           int
	}{
		{90, 0, false, 90},   // a typical back camera, phone upright
		{90, 90, false, 180}, // turned clockwise
		{90, 270, false, 0},  // turned the other way
		{270, 0, true, 270},  // a typical front camera, upright
		{270, 90, true, 180}, // front cameras turn the other way
		{270, 270, true, 0},
		{90, 180, false, 270}, // upside down
	}
	for _, c := range cases {
		if got := uprightRotation(c.sensor, c.device, c.front); got != c.want {
			t.Errorf("uprightRotation(%d, %d, front=%v) = %d, want %d", c.sensor, c.device, c.front, got, c.want)
		}
	}
	for in, want := range map[int]int{0: 0, 44: 0, 46: 90, 134: 90, 136: 180, 300: 270, 359: 0, -90: 270, 450: 90} {
		if got := quarterTurn(in); got != want {
			t.Errorf("quarterTurn(%d) = %d, want %d", in, got, want)
		}
	}
}
