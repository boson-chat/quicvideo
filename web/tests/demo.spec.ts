import { expect, test } from "@playwright/test";
import { WEB_PORT } from "./setup";

const url = (q: string) => `http://localhost:${WEB_PORT}/web/?auto=1&room=call&${q}`;

test("browsers publish and watch over WebTransport + WebCodecs", async ({ browser }, info) => {
  // H.264 WebCodecs support in this browser.
  const probe = await browser.newPage();
  await probe.goto(`http://localhost:${WEB_PORT}/web/`);
  const support = await probe.evaluate(async () => ({
    enc: (await VideoEncoder.isConfigSupported({ codec: "avc1.42e01f", width: 640, height: 360, avc: { format: "annexb" } } as VideoEncoderConfig)).supported,
    dec: (await VideoDecoder.isConfigSupported({ codec: "avc1.64002a" })).supported,
    wt: "WebTransport" in globalThis,
  }));
  expect(support).toEqual({ enc: true, dec: true, wt: true });
  await probe.close();

  // Alice: fake camera + this-tab screen share, encoded in the browser.
  const alice = await (await browser.newContext()).newPage();
  await alice.goto(url("token=alice&camera=1&screen=1"));
  await expect(alice.locator("#status")).toContainText(/in "call"|joined|sharing/, { timeout: 20_000 });

  // Bob: watches the Go bot (real H.264 from ffmpeg) and Alice's tracks.
  const bob = await (await browser.newContext()).newPage();
  await bob.goto(url("token=bob"));
  await expect.poll(async () => {
    const s = (await bob.evaluate(() => (globalThis as any).qv.stats())) as Record<string, number>;
    return Object.values(s).filter((n) => n > 20).length;
  }, { timeout: 45_000, intervals: [500] }).toBeGreaterThanOrEqual(3);

  const tiles = await bob.evaluate(() => (globalThis as any).qv.tiles());
  const labels = tiles.map((t: { label: string }) => t.label).join(" | ");
  expect(labels).toContain("go-bot · camera");
  expect(labels).toContain("alice · camera");
  expect(labels).toContain("alice · screen");
  // Alice only encodes the layers Bob watches.
  const pub = await alice.evaluate(() => (globalThis as any).qv.publishing());
  expect(pub.camera.length).toBeGreaterThan(0);
  expect(pub.camera.length).toBeLessThan(3);

  await bob.waitForTimeout(1500);
  await bob.screenshot({ path: info.outputPath("bob.png") });
  await bob.screenshot({ path: "test-results/webtransport-demo.png" });
  console.log("decoded frames per track:", JSON.stringify(await bob.evaluate(() => (globalThis as any).qv.stats())), "tiles:", labels, "alice encodes layers:", JSON.stringify(pub));
});
