package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boson-chat/quicvideo/gen/controlpb"
	"github.com/boson-chat/quicvideo/server/internal/relay"
)

type peer struct {
	id   uint32
	mu   sync.Mutex
	msgs []*controlpb.ControlMessage
}

func (p *peer) ID() uint32 { return p.id }
func (p *peer) OpenGroup(context.Context, int8) (relay.GroupWriter, error) {
	return nil, context.Canceled
}
func (p *peer) SendControl(m *controlpb.ControlMessage) {
	p.mu.Lock()
	p.msgs = append(p.msgs, m)
	p.mu.Unlock()
}
func (p *peer) kicked() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.msgs {
		if m.GetError().GetCode() == controlpb.Error_KICKED {
			return true
		}
	}
	return false
}

func TestControlAPI(t *testing.T) {
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	r := relay.New(context.Background(), relay.Options{NoLoop: true, Now: clock})
	alice, bob, carol := &peer{id: 1}, &peer{id: 2}, &peer{id: 3}
	ma, _, _ := r.Join(alice, relay.Participant{UserID: "alice"}, "general")
	_, _, _ = r.Join(bob, relay.Participant{UserID: "bob"}, "general")
	srv := httptest.NewServer(NewControlRouter(r, ControlOptions{Token: "s3cret", Grace: 15 * time.Second}))
	defer srv.Close()
	call := func(method, path, token, body string) int {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := call("POST", "/v1/control/events", "wrong", `{"events":[]}`); code != 401 {
		t.Fatalf("wrong token: %d", code)
	}
	// Alice left the voice channel.
	if code := call("POST", "/v1/control/events", "s3cret", `{"events":[{"type":"leave","room":"general","user":"alice"}]}`); code != 200 {
		t.Fatalf("events: %d", code)
	}
	if !alice.kicked() || bob.kicked() || ma.Room() != "" {
		t.Fatal("alice (only) should be removed")
	}
	// A snapshot without bob removes him, but carol joined just now: spared.
	mu.Lock()
	now = now.Add(time.Minute)
	mu.Unlock()
	_, _, _ = r.Join(carol, relay.Participant{UserID: "carol"}, "general")
	if code := call("PUT", "/v1/control/rooms", "s3cret", `{"rooms":{"general":["alice"]}}`); code != 200 {
		t.Fatalf("snapshot: %d", code)
	}
	if !bob.kicked() || carol.kicked() {
		t.Fatalf("snapshot: bob kicked=%v carol kicked=%v", bob.kicked(), carol.kicked())
	}
	// Closing the channel empties the room.
	call("POST", "/v1/control/events", "s3cret", `{"events":[{"type":"close","room":"general"}]}`)
	if !carol.kicked() {
		t.Fatal("close should remove everyone")
	}
	if rooms, _ := r.Stats(); rooms != 0 {
		t.Fatalf("%d rooms left", rooms)
	}
	// Alice may join again (the session isn't stuck "in a room").
	if _, _, err := r.Join(alice, relay.Participant{UserID: "alice"}, "general"); err != nil {
		t.Fatal(err)
	}
}
