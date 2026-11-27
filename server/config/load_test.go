package config

import "testing"

// With nothing set, every tag default applies (the image starts that way).
func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("WEBTRANSPORT_ORIGINS", "")
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	q := c.QUICConfig
	if q.WebTransportOrigins != "*" {
		t.Errorf("origins %q", q.WebTransportOrigins)
	}
	if q.MaxStreamReceiveWindow != 8388608 || q.MaxConnectionReceiveWindow != 33554432 {
		t.Errorf("windows %d %d", q.MaxStreamReceiveWindow, q.MaxConnectionReceiveWindow)
	}
}
