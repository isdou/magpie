// Run with Node's test runner and Playwright on the module path; see README.md.
// Relocating a Codex project: the folder's ⋯ menu offers Relocate project on
// Codex project folders, previews via POST /api/sessions/relocate-codex without
// a token, confirms with the preview token, and reloads under the new path.
// All 5 languages, Chromium and WebKit, wide and narrow viewports.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const now = new Date(Date.now() - 120e3).toISOString();
const row = (cwd) => ({
  agent: "codex", id: "019a7777-7777-7777-8777-777777777777", cwd, title: "Fix Codex bug",
  start: now, last: now, input: 1000, output: 200, cost: 0.02, priced: true,
  models: [{ model: "gpt-5.5", input: 1000, output: 200, cost: 0.02, priced: true }],
  messages: 4, files: 1, size: 2048, deletable: true, resume: "codex resume 019a7777-7777-7777-8777-777777777777",
});
const agents = [{ agent: "codex", name: "Codex", icon: "openai", count: 1, deletable: true }];

const keys = [
  "Relocate project",
  "Relocate Codex project",
  "New absolute project path",
  "Preview relocation",
  "Confirm relocation",
  "Close Codex and its editor sessions first. This moves every session of the project in Codex's store, not just filtered or selected rows. Rollouts whose history is paginated cannot move to a longer path.",
  "Move {n} sessions ({size}) from {from} to {to}. Originals are backed up before update. Historical message text is unchanged.",
  "Relocation complete. Original files and recovery manifest: {dir}",
];

function serve(lang, state, posts) {
  return async (route) => {
    const req = route.request();
    const url = new URL(req.url());
    const json = (data, status = 200) => route.fulfill({ json: data, status });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:false};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light" } });
    if (url.pathname === "/api/plugins") return json({ plugins: [] });
    if (url.pathname === "/api/groups") return json({ groups: [] });
    if (url.pathname === "/api/providers") return json({ providers: [], gateway: { running: true, window: true } });
    if (url.pathname === "/api/sessions/manage") return json({ agents, agent: "codex", sessions: [row(state.cwd)], trash: [] });
    if (url.pathname === "/api/sessions/relocate-codex") {
      const body = req.postDataJSON();
      posts.push(body);
      if (!body.token) {
        return json({ from: body.from, to: body.to, sessions: 1, files: 1, bytes: 2048, token: "cx-preview-token" });
      }
      state.cwd = body.to;
      return json({
        from: body.from, to: body.to, sessions: 1, files: 1, bytes: 2048,
        token: body.token, backup: "/home/me/.codex/magpie-relocations/codex-project-1",
      });
    }
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    const body = await fs.readFile(file).catch(() => null);
    return body ? route.fulfill({ body, contentType }) : route.fulfill({ status: 404, body: "" });
  };
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
    for (const width of [1000, 420]) {
      test(`${engine} ${lang} ${width}px: a Codex folder is relocated after a preview`, async (t) => {
        const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
        t.after(() => browser.close());
        const page = await (await browser.newContext({ viewport: { width, height: 680 }, reducedMotion: "reduce" })).newPage();
        page.setDefaultTimeout(8000);
        page.setDefaultNavigationTimeout(60000);
        const errors = [], posts = [];
        const state = { cwd: "/work/old-codex" };
        page.on("pageerror", (e) => errors.push(e.message));
        await page.route("**/*", serve(lang, state, posts));
        await page.goto("http://magpie.test/");
        await page.locator('#nav [data-view="sessions"]').click();

        const view = page.locator("#view-sessions");
        await view.locator(".sm-folder .fold .name", { hasText: "old-codex" }).waitFor();
        const tr = (k, vars) => page.evaluate(([lang, k, vars]) => {
          let s = (typeof I18N !== "undefined" && I18N[lang]?.[k]) || k;
          if (vars) for (const [a, b] of Object.entries(vars)) s = s.replaceAll(`{${a}}`, String(b));
          return s;
        }, [lang, k, vars]);
        if (lang !== "en") {
          for (const k of keys) {
            assert.notEqual(await tr(k), k, `${lang} translates ${k}`);
          }
        }

        await view.locator(".sm-folder .sm-folder-more").click();
        await page.locator(".pop.row-menu .rm-item", { hasText: await tr("Relocate project") }).click();
        const ask = page.locator("#modal .sm-ask");
        await ask.waitFor();
        assert.equal((await ask.locator(".ehead b").textContent()).trim(), await tr("Relocate Codex project"));
        await ask.locator("input").fill("/work/new-codex");
        await ask.locator(".bar button.primary").click();

        await ask.locator("p.lib-confirm", { hasText: "cx-preview-token" }).count();
        await ask.locator(".bar button.primary", { hasText: await tr("Confirm relocation") }).waitFor();
        await ask.locator(".bar button.primary").click();
        await ask.locator(".bar button.primary").waitFor({ state: "hidden" });
        await ask.locator(".bar button", { hasText: await tr("Close") }).click();
        await ask.waitFor({ state: "detached" });

        await view.locator(".sm-folder .fold .name", { hasText: "new-codex" }).waitFor();
        assert.deepEqual(posts, [
          { from: "/work/old-codex", to: "/work/new-codex", wsl: "", token: "" },
          { from: "/work/old-codex", to: "/work/new-codex", wsl: "", token: "cx-preview-token" },
        ]);
        assert.deepEqual(errors, []);
        await page.context().close();
      });
    }
  }
}
