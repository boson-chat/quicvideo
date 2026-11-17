# Screen share sound

A screen share can carry the sound of what's shared: the whole system
(a display share) or one app (a window share). Viewers hear it in sync
with the shared picture and can turn it down or mute it per share.
macOS 13+ sends it (ScreenCaptureKit audio); every client can play it.

## Options

**(a) An Opus track in quicvideo, part of the screen share (chosen).**
A `SCREEN_AUDIO` track, published next to the `SCREEN` track it belongs
to (`for_track_id`), relayed like any other track. Receivers play it
through the voice engine's mixer.

**(b) A second audio source per session in quicvoice.** The sharer's
voice connection would send a second Opus stream, and quicvoice would
carry and mix it as if it were another speaker.

## Why (a)

- **Lip sync with the screen.** Screen audio and screen video are
  stamped with the same capture clock (`capture_ts_us`, the sharer's
  wall clock) by the same capture session, and they reach the viewer
  over the same connection with the same priority. Lip sync is the
  existing mechanism: the viewer's mixer reports which capture time is
  at the speaker now, and the screen's frames are shown at capture time
  plus that offset (the `sync` offsets the page already uses, now per
  track for a screen with sound). In (b) the sound would cross a
  different server and connection from its picture, and would need an
  AudioClock bridge between the two clocks, just to get back what (a)
  has for free.
- **The same group and priority model.** The sound is a one-layer track
  at screen urgency (1), so the bandwidth controller always keeps it
  (as it does a screen share's lowest layer), and it's dropped with its
  share. Nobody gets it who isn't in the video room, and there's no
  voice-server change or quicvoice protocol change.
- **No new mixing path.** The viewer's client feeds the frames into the
  voice engine's mixer as one more stream (its own jitter buffer, gain
  and playout delay), so volume, deafen, the output device and phones'
  audio routes all apply as they do for voices.

## Echo

- The sharer never hears it back: it isn't subscribed to its own track,
  and ScreenCaptureKit leaves Boson's own process out of the capture
  (`excludesCurrentProcessAudio`), so the call's voices aren't re-sent.
- A sharer on speakers rather than headphones: their mic still hears
  the room, the same as a voice call today; nothing here changes the
  voice path.

## Wire

- `TrackKind SCREEN_AUDIO = 3`; `Publish.for_track_id` and
  `Track.for_track_id` name the publisher's own `SCREEN` track. One
  layer (`id 0`), codec `opus`, about 64 kbit/s.
- Frames: one Opus packet per 20 ms (48 kHz mono, the voice engine's
  format). Every packet decodes on its own; the first of each group is
  flagged keyframe. A group is about 200 ms (10 frames), so a dropped
  group costs 200 ms of sound, not more, and a keyframe request just
  starts the next group early.
- `capture_ts_us` of each frame is its first sample's capture time,
  counted from the sample position (so frames are exactly 20 ms apart);
  a gap restarts the count.
- The relay treats it as part of the screen share: default urgency 1,
  always allocated, never paused.
- Capability `screen-audio`: the server lists it in `Welcome`
  (clients only offer "Share sound" where it does), and a client lists
  it in `Hello` to hear about sound tracks at all. Older clients never
  see them: they'd take an unknown track for video and show an empty
  tile.

## Clients

- **Sending (macOS 13+):** the picker's "Share sound" (per share, off by
  default). SCK delivers 48 kHz audio with the screen's stream; the
  client downmixes to mono, encodes 20 ms Opus frames (`AppAudio`) and
  publishes them. Switching the source keeps the sound track.
  Elsewhere the toggle stays hidden (`canShareAudio`).
- **Receiving (all):** while the call view is open, a client subscribes
  to the sound of every screen share and feeds it to its mixer as a
  stream of its own. Each share has its own volume and mute.
