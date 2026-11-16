import { expect, type Page, test } from "@playwright/test";
import { WEB_PORT } from "./setup";

const url = (q: string) => `http://localhost:${WEB_PORT}/web/?auto=1&room=call&${q}`;

interface Support {
  watch: boolean;
  send: boolean;
  missing: string[];
}

async function support(page: Page): Promise<Support> {
  await page.goto(`http://localhost:${WEB_PORT}/web/`);
  return page.evaluate(() => (globalThis as any).qv.support());
}

test("unsupported browsers say so instead of failing", async ({ browser }, info) => {
  const page = await browser.newPage();
  const s = await support(page);
  console.log(`${info.project.name}: support ${JSON.stringify(s)}`);
  if (s.watch) {
    await expect(page.locator("#unsupported")).toBeHidden();
    await expect(page.locator("#join")).toBeEnabled();
  } else {
    await expect(page.locator("#unsupported")).toBeVisible();
    await expect(page.locator("#unsupported")).toContainText("isn't supported for video");
    await expect(page.locator("#join")).toBeDisabled();
    await page.screenshot({ path: `test-results/unsupported-${info.project.name}.png` });
  }
});

test("browsers publish and watch over WebTransport + WebCodecs", async ({ browser }, info) => {
  const probe = await browser.newPage();
  const s = await support(probe);
  await probe.close();
  test.skip(!s.watch, `${info.project.name} lacks ${s.missing.join(", ")}`);

  // Only Chromium can auto-accept a screen share in automation.
  const screen = info.project.name === "chromium" && s.send;
  // WebKit has fake capture devices but needs the permission granted.
  const perms = info.project.name === "webkit" ? { permissions: ["camera"] } : {};
  const alice = await (await browser.newContext(perms)).newPage();
  await alice.goto(url(`token=alice-${info.project.name}&camera=${s.send ? 1 : 0}&screen=${screen ? 1 : 0}`));
  await expect(alice.locator("#status")).toContainText(/in "call"|joined|sharing/, { timeout: 20_000 });

  // Bob watches the Go bot (real H.264 from ffmpeg) and Alice's tracks.
  const want = 1 + (s.send ? 1 : 0) + (screen ? 1 : 0);
  const bob = await (await browser.newContext()).newPage();
  await bob.goto(url(`token=bob-${info.project.name}`));
  await expect.poll(async () => {
    const st = (await bob.evaluate(() => (globalThis as any).qv.stats())) as Record<string, number>;
    return Object.values(st).filter((n) => n > 20).length;
  }, { timeout: 45_000, intervals: [500] }).toBeGreaterThanOrEqual(want);

  const tiles = await bob.evaluate(() => (globalThis as any).qv.tiles());
  const labels = tiles.map((t: { label: string }) => t.label).join(" | ");
  expect(labels).toContain("go-bot · camera");
  if (s.send) {
    expect(labels).toContain(`alice-${info.project.name} · camera`);
    // Alice only encodes the layers Bob watches.
    const pub = await alice.evaluate(() => (globalThis as any).qv.publishing());
    expect(pub.camera.length).toBeGreaterThan(0);
    expect(pub.camera.length).toBeLessThan(3);
  }
  if (screen) expect(labels).toContain(`alice-${info.project.name} · screen`);

  await bob.waitForTimeout(1500);
  await bob.screenshot({ path: info.outputPath("bob.png") });
  await bob.screenshot({ path: `test-results/webtransport-${info.project.name}.png` });
  console.log(`${info.project.name}: decoded frames per track:`, JSON.stringify(await bob.evaluate(() => (globalThis as any).qv.stats())), "tiles:", labels);
  await alice.context().close();
  await bob.context().close();
});

// A browser without WebTransport (simulated: older Safari, Firefox < 114)
// gets the message, a disabled Join, and no connection attempt.
test("a browser without WebTransport is told so", async ({ browser }, info) => {
  const ctx = await browser.newContext();
  await ctx.addInitScript(() => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    delete (globalThis as any).WebTransport;
  });
  const page = await ctx.newPage();
  await page.goto(`http://localhost:${WEB_PORT}/web/?auto=1&room=call&token=nowt`);
  await expect(page.locator("#unsupported")).toContainText("isn't supported for video (missing: WebTransport)");
  await expect(page.locator("#join")).toBeDisabled();
  await expect(page.locator("#status")).toHaveText("unsupported browser");
  await page.screenshot({ path: `test-results/unsupported-${info.project.name}.png` });
  await ctx.close();
});
