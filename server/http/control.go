package http

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	nethttp "net/http"
	"strings"
	"time"

	"github.com/boson-chat/quicvideo/server/internal/relay"
)

// ControlOptions configure the control listener quicvoiced talks to.
type ControlOptions struct {
	Token  string
	Strict bool // snapshot: empty rooms voice doesn't list
	Grace  time.Duration
}

type controlEvent struct {
	Type string `json:"type"` // "leave" | "close"
	Room string `json:"room"`
	User string `json:"user,omitempty"`
}

// NewControlRouter serves the voice → video control API:
//
//	POST /v1/control/events  {"events":[{"type":"leave","room":"r","user":"u"},{"type":"close","room":"r"}]}
//	PUT  /v1/control/rooms   {"rooms":{"r":["u1","u2"]}}  (full membership snapshot)
//
// Both need "Authorization: Bearer <CONTROL_TOKEN>".
func NewControlRouter(r *relay.Relay, o ControlOptions) nethttp.Handler {
	if o.Grace <= 0 {
		o.Grace = 15 * time.Second
	}
	auth := func(next nethttp.HandlerFunc) nethttp.HandlerFunc {
		return func(w nethttp.ResponseWriter, req *nethttp.Request) {
			got := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			if o.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(o.Token)) != 1 {
				nethttp.Error(w, "unauthorized", nethttp.StatusUnauthorized)
				return
			}
			req.Body = nethttp.MaxBytesReader(w, req.Body, 4<<20)
			next(w, req)
		}
	}
	mux := nethttp.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("POST /v1/control/events", auth(func(w nethttp.ResponseWriter, req *nethttp.Request) {
		var body struct {
			Events []controlEvent `json:"events"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			nethttp.Error(w, "bad json", nethttp.StatusBadRequest)
			return
		}
		removed := 0
		for _, e := range body.Events {
			switch e.Type {
			case "leave":
				removed += r.KickUser(e.Room, e.User, "left the voice channel")
			case "close":
				removed += r.CloseRoom(e.Room, "the voice channel was closed")
			}
		}
		writeJSON(w, map[string]int{"removed": removed})
	}))
	mux.HandleFunc("PUT /v1/control/rooms", auth(func(w nethttp.ResponseWriter, req *nethttp.Request) {
		var body struct {
			Rooms map[string][]string `json:"rooms"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Rooms == nil {
			nethttp.Error(w, "bad json", nethttp.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]int{"removed": r.Sync(body.Rooms, o.Strict, o.Grace, "not in the voice channel")})
	}))
	return mux
}

func writeJSON(w nethttp.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// StartControlServerWithContext serves the control router until ctx ends.
func StartControlServerWithContext(ctx context.Context, addr string, r *relay.Relay, o ControlOptions) error {
	return serve(ctx, "control HTTP", &nethttp.Server{
		Addr: addr, Handler: NewControlRouter(r, o), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second,
	})
}
