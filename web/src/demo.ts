// The quicvideo browser demo: join a room, share camera/screen, watch the
// others. Query parameters (for tests and links): token, room, server,
// auto=1 (join at once), camera=1, screen=1.
import { QuicVideoClient, type Publication, TrackKind, type Track, type VideoSupport, videoSupport } from "./client";
import { cameraLayers, layerForTile, layout, SCREEN_FPS, screenLayers } from "./layers";

const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;
const params = new URLSearchParams(location.search);
const status = (s: string) => ($("status").textContent = s);

interface Tile {
  track: Track;
  el: HTMLDivElement;
  canvas: HTMLCanvasElement;
  label: HTMLSpanElement;
  subscribed: boolean;
  maxLayer: number;
  layerText: string;
}

let client: QuicVideoClient | null = null;
let camera: Publication | null = null;
let screen: Publication | null = null;
let pinned = 0;
const tiles = new Map<number, Tile>();
const names = new Map<number, string>();

async function serverInfo(): Promise<{ url: string; certHash?: string }> {
  const info = await fetch("/v1/info").then((r) => r.json()).catch(() => ({}));
  const url = params.get("server") ?? `https://${location.hostname}:${info.quic_port || 7710}/wt`;
  return { url, certHash: info.dev_cert_hash || undefined };
}

let support: VideoSupport | null = null;

// Browsers without WebTransport or H.264 WebCodecs can't take part: say so
// plainly instead of failing on join.
async function checkSupport(): Promise<VideoSupport> {
  support ??= await videoSupport();
  const box = $("unsupported");
  if (!support.watch) {
    box.textContent = `This browser isn't supported for video (missing: ${support.missing.join(", ")}). ` +
      "Use a recent Chrome, Edge or Firefox, or Safari 26.4+.";
    box.style.display = "block";
    ($("join") as HTMLButtonElement).disabled = true;
    status("unsupported browser");
  } else if (!support.send) {
    box.textContent = `This browser can watch but not send video (missing: ${support.missing.join(", ")}).`;
    box.style.display = "block";
  }
  return support;
}

async function join(): Promise<void> {
  if (!(await checkSupport()).watch) return;
  const token = params.get("token") ?? ($("token") as HTMLInputElement).value;
  const room = params.get("room") ?? ($("room") as HTMLInputElement).value;
  status("connecting…");
  const { url, certHash } = await serverInfo();
  client = await QuicVideoClient.connect(url, token, { certHash });
  client.on("track", addTrack);
  client.on("trackRemoved", removeTrack);
  client.on("participant", (id, name, joined) => {
    if (joined) names.set(id, name);
    status(`${joined ? name : names.get(id) ?? "someone"} ${joined ? "joined" : "left"}`);
  });
  client.on("state", (s) => {
    const t = tiles.get(s.trackId);
    if (!t) return;
    const l = t.track.layers.find((l) => l.id === s.layer);
    t.layerText = s.paused ? " · paused (bandwidth)" : l && s.forwarding ? ` · ${l.height}p` : "";
    relabel(t);
  });
  client.on("closed", (r) => status(`disconnected: ${r}`));
  const st = await client.join(room);
  for (const p of st.participants) names.set(p.sessionId, p.displayName);
  st.tracks.forEach(addTrack);
  status(`in "${room}" with ${st.participants.length} other(s)`);
  ($("join") as HTMLButtonElement).disabled = true;
  for (const b of support!.send ? ["camera", "screen", "leave"] : ["leave"]) ($(b) as HTMLButtonElement).disabled = false;
  relayout();
}

function relabel(t: Tile): void {
  const who = names.get(t.track.sessionId) ?? `session ${t.track.sessionId}`;
  t.label.textContent = `${who} · ${t.track.kind === TrackKind.SCREEN ? "screen" : "camera"}${t.layerText}`;
}

function addTrack(track: Track): void {
  if (tiles.has(track.trackId)) return;
  const el = document.createElement("div");
  el.className = "tile";
  const canvas = document.createElement("canvas");
  const label = document.createElement("span");
  label.className = "label";
  el.append(canvas, label);
  el.onclick = () => {
    pinned = pinned === track.trackId ? 0 : track.trackId;
    relayout();
  };
  $("stage").insertBefore(el, $("self"));
  const t: Tile = { track, el, canvas, label, subscribed: false, maxLayer: -1, layerText: "" };
  tiles.set(track.trackId, t);
  if (!pinned && track.kind === TrackKind.SCREEN) pinned = track.trackId;
  relabel(t);
  relayout();
}

function removeTrack(id: number): void {
  tiles.get(id)?.el.remove();
  tiles.delete(id);
  if (pinned === id) pinned = 0;
  relayout();
}

function draw(t: Tile, f: VideoFrame): void {
  const c = t.canvas;
  const dpr = devicePixelRatio || 1;
  const w = Math.round(c.clientWidth * dpr);
  const h = Math.round(c.clientHeight * dpr);
  if (c.width !== w || c.height !== h) {
    c.width = w;
    c.height = h;
  }
  const ctx = c.getContext("2d")!;
  const s = Math.min(w / f.displayWidth, h / f.displayHeight);
  const dw = f.displayWidth * s;
  const dh = f.displayHeight * s;
  ctx.clearRect(0, 0, w, h);
  ctx.drawImage(f, (w - dw) / 2, (h - dh) / 2, dw, dh);
  f.close();
}

// Subscriptions follow the layout: the layer that fits each tile, nothing
// for hidden tiles or a hidden page.
function relayout(): void {
  const stage = $("stage");
  const rects = layout([...tiles.keys()], pinned, stage.clientWidth, stage.clientHeight);
  const dpr = devicePixelRatio || 1;
  for (const [id, r] of rects) {
    const t = tiles.get(id)!;
    const visible = !!r && document.visibilityState === "visible";
    t.el.style.display = r ? "" : "none";
    if (r) Object.assign(t.el.style, { left: `${r.x}px`, top: `${r.y}px`, width: `${r.w}px`, height: `${r.h}px` });
    const layer = r ? layerForTile(t.track.layers, r.w * dpr, r.h * dpr, visible) : null;
    if (!client) continue;
    if (layer !== null && (!t.subscribed || t.maxLayer !== layer)) {
      t.subscribed = true;
      t.maxLayer = layer;
      void client.subscribe(t.track, layer, (f) => draw(t, f));
    } else if (layer === null && t.subscribed) {
      t.subscribed = false;
      void client.unsubscribe(id);
    }
  }
}

async function toggleCamera(): Promise<void> {
  if (camera) {
    camera.stop();
    camera = null;
    $("self").style.display = "none";
    $("camera").textContent = "Camera on";
    return;
  }
  const media = await navigator.mediaDevices.getUserMedia({ video: { width: 1280, height: 720, frameRate: 30 } });
  const track = media.getVideoTracks()[0];
  const s = track.getSettings();
  camera = await client!.publish(TrackKind.CAMERA, track, cameraLayers(s.width ?? 1280, s.height ?? 720, Math.round(s.frameRate ?? 30)));
  ($("self").querySelector("video") as HTMLVideoElement).srcObject = media;
  $("self").style.display = "block";
  $("camera").textContent = "Camera off";
}

async function toggleScreen(): Promise<void> {
  if (screen) {
    screen.stop();
    screen = null;
    $("screen").textContent = "Share screen";
    return;
  }
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const media = await navigator.mediaDevices.getDisplayMedia({ video: { frameRate: SCREEN_FPS }, preferCurrentTab: params.has("screen") } as any);
  const track = media.getVideoTracks()[0];
  const s = track.getSettings();
  screen = await client!.publish(TrackKind.SCREEN, track, screenLayers(s.width ?? 1920, s.height ?? 1080));
  track.onended = () => void (screen && toggleScreen());
  $("screen").textContent = "Stop sharing";
}

$("join").onclick = () => join().catch((e) => status(`error: ${e.message ?? e}`));
$("camera").onclick = () => toggleCamera().catch((e) => status(`camera: ${e.message ?? e}`));
$("screen").onclick = () => toggleScreen().catch((e) => status(`screen: ${e.message ?? e}`));
$("leave").onclick = () => {
  client?.close();
  status("left");
  for (const b of ["camera", "screen", "leave"]) ($(b) as HTMLButtonElement).disabled = true;
};
addEventListener("resize", relayout);
document.addEventListener("visibilitychange", relayout);

// For tests and debugging.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
(globalThis as any).qv = {
  stats: () => client?.stats() ?? {},
  publishing: () => ({ camera: camera?.activeLayers() ?? null, screen: screen?.activeLayers() ?? null }),
  tiles: () => [...tiles.values()].map((t) => ({ id: t.track.trackId, label: t.label.textContent })),
  support: () => checkSupport(),
};

void checkSupport().then((s) => {
  if (params.get("auto") !== "1" || !s.watch) return;
  join()
    .then(async () => {
      if (!s.send) return;
      if (params.get("camera") === "1") await toggleCamera();
      if (params.get("screen") === "1") await toggleScreen();
    })
    .catch((e) => status(`error: ${e.message ?? e}`));
});
