// Starts quicvideod (WebTransport on, dev certificate) and a Go bot that
// publishes real H.264 (the ffmpeg test pattern) for the browser to decode.
import { execFileSync, spawn } from "node:child_process";
import { existsSync, mkdirSync } from "node:fs";
import path from "node:path";

export const QUIC_PORT = 17721;
export const WEB_PORT = 18291;
const repo = path.resolve(path.dirname(new URL(import.meta.url).pathname), "../..");

async function waitFor(url: string, ms: number): Promise<void> {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    try {
      if ((await fetch(url)).ok) return;
    } catch {}
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(`${url} not up`);
}

export default async function globalSetup(): Promise<() => void> {
  const bin = path.join(repo, "web/test-results/bin");
  mkdirSync(bin, { recursive: true });
  execFileSync("go", ["build", "-o", bin, "./server/cmd", "./examples/pub"], { cwd: repo, stdio: "inherit" });
  if (!existsSync(path.join(repo, "testdata/l2.h264"))) execFileSync("make", ["testdata"], { cwd: repo, stdio: "inherit" });
  const server = spawn(path.join(bin, "cmd"), ["serve"], {
    env: {
      ...process.env, AUTH_INSECURE: "true", ENV: "production",
      QUIC_ADDR: `:${QUIC_PORT}`, HTTP_ADDR: "127.0.0.1:18281", PUBLIC_HTTP_ADDR: `:${WEB_PORT}`,
    },
    stdio: "inherit",
  });
  await waitFor(`http://localhost:${WEB_PORT}/healthz`, 15_000);
  const bot = spawn(path.join(bin, "pub"), [
    "-addr", `127.0.0.1:${QUIC_PORT}`, "-insecure", "-token", "go-bot", "-room", "call",
    "-layers", "320x180@15:200k,640x360@30:600k,1280x720@30:1500k",
    "-h264", ["l0", "l1", "l2"].map((l) => path.join(repo, `testdata/${l}.h264`)).join(","),
  ], { stdio: "ignore" });
  return () => {
    bot.kill();
    server.kill();
  };
}
