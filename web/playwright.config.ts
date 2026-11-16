import { defineConfig, devices } from "@playwright/test";

const viewport = { width: 1280, height: 800 };

export default defineConfig({
  testDir: "tests",
  timeout: 90_000,
  globalSetup: "./tests/setup.ts",
  // One server + bot for all browsers: run them one after another.
  workers: 1,
  projects: [
    {
      name: "chromium",
      use: {
        // Full Chromium (not the headless shell): WebCodecs H.264 + WebTransport.
        channel: "chromium",
        launchOptions: {
          args: ["--use-fake-ui-for-media-stream", "--use-fake-device-for-media-stream", "--auto-accept-this-tab-capture"],
        },
        viewport,
      },
    },
    {
      name: "firefox",
      use: {
        ...devices["Desktop Firefox"],
        viewport,
        launchOptions: {
          firefoxUserPrefs: {
            "media.navigator.streams.fake": true,
            "media.navigator.permission.disabled": true,
          },
        },
      },
    },
    { name: "webkit", use: { ...devices["Desktop Safari"], viewport } },
  ],
  reporter: [["list"]],
});
