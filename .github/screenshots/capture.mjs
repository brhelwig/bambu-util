// Renders the page in each state the printer can be in and writes a PNG per
// state.
//
// The states are driven by answering /api/status in the browser rather than by
// pretending at the printer, so every screenshot is the real page and the real
// script reacting to a status payload it cannot tell from a live one.
import { chromium } from "playwright";
import { mkdir } from "node:fs/promises";

const BASE = process.env.BASE || "http://127.0.0.1:8081";
const OUT = process.env.OUT || "screenshots";

// A phone, which is what this page is for.
const VIEWPORT = { width: 390, height: 844 };

const idle = {
  connected: true,
  gcodeState: "IDLE",
  actionsAllowed: true,
  bedTemp: 22.4,
  bedTarget: 0,
  nozzleTemp: 23.1,
  nozzleTarget: 0,
  chamberLight: true,
  fans: { cooling: 0, aux: 0, chamber: 0 },
  printActions: { pause: false, resume: false, stop: false },
  bedOffIn: null,
  nozzleOffIn: null,
  lampOffIn: 27180,
  ams: null,
  hms: null,
};

const ams = {
  tray_now: "1",
  ams: [{
    id: "0",
    humidity: "4",
    tray: [
      { id: "0", tray_type: "PLA", tray_color: "FF6B35FF", nozzle_temp_min: "190", nozzle_temp_max: "230", tray_info_idx: "GFA00" },
      { id: "1", tray_type: "PETG", tray_color: "2E86ABFF", nozzle_temp_min: "220", nozzle_temp_max: "260", tray_info_idx: "GFG00" },
      { id: "2", tray_type: "ABS", tray_color: "111418FF", nozzle_temp_min: "240", nozzle_temp_max: "270", tray_info_idx: "GFB00" },
      { id: "3", tray_type: "PLA", tray_color: "7BD88FFF", nozzle_temp_min: "190", nozzle_temp_max: "230", tray_info_idx: "GFA00" },
    ],
  }],
};

const printing = {
  ...idle,
  gcodeState: "RUNNING",
  actionsAllowed: false,
  bedTemp: 59.8,
  bedTarget: 60,
  nozzleTemp: 219.4,
  nozzleTarget: 220,
  progress: 47,
  jobName: "Minimalist_Pencil_Holder.3mf",
  layerNum: 88,
  totalLayerNum: 187,
  remainingMinutes: 132,
  fans: { cooling: 85, aux: 40, chamber: 0 },
  printActions: { pause: true, resume: false, stop: true },
  ams,
};

const states = [
  {
    name: "01-idle",
    title: "Idle",
    note: "Nothing printing. Every manual action is available.",
    status: { ...idle, ams },
  },
  {
    name: "02-printing",
    title: "Printing",
    note: "Mid-print: progress, layer and time remaining appear, and the bed and filament controls are refused.",
    status: printing,
  },
  {
    name: "03-paused",
    title: "Paused",
    note: "A paused print is still the same print — Resume replaces Pause, and it is not reported as ended.",
    status: {
      ...printing,
      gcodeState: "PAUSE",
      printActions: { pause: false, resume: true, stop: true },
    },
  },
  {
    name: "04-finished",
    title: "Finished",
    note: "The print completed. Actions are allowed again, so filament can be unloaded.",
    status: {
      ...idle, ams,
      gcodeState: "FINISH",
      jobName: "Minimalist_Pencil_Holder.3mf",
      bedTemp: 41.2,
      nozzleTemp: 132.6,
    },
  },
  {
    name: "05-ended-without-finishing",
    title: "Ended without finishing",
    note: "Stopped by hand, or the printer gave up — it reports the same state either way, so the page does not claim to know which.",
    status: { ...idle, ams, gcodeState: "FAILED", jobName: "Minimalist_Pencil_Holder.3mf" },
  },
  {
    name: "06-printer-error",
    title: "Printer error",
    note: "An alert the printer raised. Filament runout arrives this way rather than as a field of its own.",
    status: {
      ...printing,
      hms: [{ code: "0300-8000-0003-0002", message: "AMS filament runout" }],
    },
  },
  {
    name: "07-drying",
    title: "Bed drying",
    note: "The bed heated with nothing printing, showing the safety shut-off counting down.",
    status: {
      ...idle, ams,
      bedTemp: 58.9,
      bedTarget: 60,
      bedOffIn: 81900,
    },
  },
  {
    name: "08-disconnected",
    title: "Disconnected",
    note: "The printer is unreachable. Every action is refused rather than sent into the dark.",
    status: {
      ...idle,
      connected: false,
      actionsAllowed: false,
      chamberLight: null,
      lampOffIn: null,
    },
  },
];

async function main() {
  await mkdir(OUT, { recursive: true });
  const browser = await chromium.launch();
  const context = await browser.newContext({
    viewport: VIEWPORT,
    deviceScaleFactor: 2,
    colorScheme: "dark",
  });

  const shots = [];
  for (const state of states) {
    const page = await context.newPage();
    // Answer the status endpoint ourselves; everything else reaches the real
    // server, so the camera card shows its genuine empty state rather than a
    // staged one.
    await page.route("**/api/status", route =>
      route.fulfill({ json: state.status }));
    await page.goto(BASE + "/", { waitUntil: "networkidle" });
    await page.waitForTimeout(400);
    const file = `${OUT}/${state.name}.png`;
    await page.screenshot({ path: file, fullPage: true });
    shots.push({ ...state, file });
    console.log(`captured ${state.title}`);
    await page.close();
  }

  // The settings screen, reached the way a person reaches it.
  {
    const page = await context.newPage();
    await page.route("**/api/status", route => route.fulfill({ json: { ...idle, ams } }));
    await page.goto(BASE + "/", { waitUntil: "networkidle" });
    await page.click("#settingsBtn");
    await page.waitForTimeout(400);
    const file = `${OUT}/09-settings.png`;
    await page.screenshot({ path: file, fullPage: true });
    shots.push({
      name: "09-settings",
      title: "Settings",
      note: "Its own screen behind the gear, not more cards under the printer status.",
      file,
    });
    console.log("captured Settings");
    await page.close();
  }

  await browser.close();
  console.log(JSON.stringify(shots.map(s => ({ name: s.name, title: s.title, note: s.note })), null, 2));
}

await main();
