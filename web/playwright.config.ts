import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "tests",
  timeout: 90_000,
  globalSetup: "./tests/setup.ts",
  use: {
    // Full Chromium (not the headless shell): WebCodecs H.264 + WebTransport.
    channel: "chromium",
    launchOptions: {
      args: [
        "--use-fake-ui-for-media-stream",
        "--use-fake-device-for-media-stream",
        "--auto-accept-this-tab-capture",
      ],
    },
    viewport: { width: 1280, height: 800 },
  },
  reporter: [["list"]],
});
