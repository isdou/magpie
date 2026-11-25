// Run with Node's test runner and Playwright on the module path; see README.md.
// bb's per-thread personal workspaces group under Draft sessions across agents,
// linked git worktrees group under their main repository (showing the worktree
// path on row hover), and deleted bb worktrees without worktree_of fall back to
// Draft sessions rather than cluttering Other folders (#7598). All 5 languages,
// Chromium and WebKit, wide and narrow.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const now = (m) => new Date(Date.now() - m * 60e3).toISOString();
const row = (agent, id, cwd, extra = {}) => ({
  agent, id, cwd, title: id,
  start: now(10), last: now(1), input: 100, output: 20, cost: 0.01, priced: true,
  models: [{ model: agent === "codex" ? "gpt-5.5" : "claude-sonnet-4-6", input: 100, output: 20, cost: 0.01, priced: true }],
  messages: 2, files: 1, size: 1024, deletable: true, resume: `${agent} --resume ${id}`,
  ...extra,
});

const claudeSessions = [
  // Main repository session + bb worktree + .claude/worktrees/feat share "app".
  row("claude", "cc-main", "/Users/me/code/app", { last: now(1) }),
  row("claude", "cc-bb-wt", "/Users/me/.bb-machines/devbox/plugins/environment-git-worktree/host-data/worktrees/thr_wt1-0/app", { worktree_of: "/Users/me/code/app", last: now(2) }),
  row("claude", "cc-cl-wt", "/Users/me/code/app/.claude/worktrees/feat", { worktree_of: "/Users/me/code/app", last: now(3) }),
  // Group consisting only of a linked worktree (no main checkout sessions) does not offer Relocate project.
  row("claude", "cc-only-wt", "/home/me/.bb-machines/linuxbox/plugins/environment-git-worktree/host-data/worktrees/thr_wt2-0/lib", { worktree_of: "/home/me/code/lib", last: now(4) }),
  // Personal workspace and deleted bb worktree go into Draft sessions.
  row("claude", "cc-bb-draft", "/Users/me/.bb-machines/devbox/plugins/environment-personal-workspace/host-data/workspaces/thr_p1", { last: now(5) }),
  row("claude", "cc-bb-gone", "/home/me/.bb-machines/linuxbox/plugins/environment-git-worktree/host-data/worktrees/thr_gone-0/ghost", { last: now(6) }),
  // Ordinary hidden-home folder still waits in Other folders.
  row("claude", "cc-scratch", "/Users/me/.cache/scratch", { last: now(7) }),
];

const codexSessions = [
  row("codex", "cx-wt-resolved", "/Users/me/.codex/worktrees/ab12/app", { worktree_of: "/Users/me/code/app", last: now(1) }),
  row("codex", "cx-bb-draft", "/Users/me/.bb-machines/devbox/plugins/environment-personal-workspace/host-data/workspaces/thr_p2", { last: now(2) }),
];

const agents = [
  { agent: "claude", name: "Claude Code", icon: "claudecode-color", count: claudeSessions.length, deletable: true },
  { agent: "codex", name: "Codex", icon: "openai", count: codexSessions.length, deletable: true },
];

function serve(lang) {
  return async (route) => {
    const url = new URL(route.request().url());
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:false};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light" } });
    if (url.pathname === "/api/plugins") return json({ plugins: [] });
    if (url.pathname === "/api/groups") return json({ groups: [] });
    if (url.pathname === "/api/providers") return json({ providers: [], gateway: { running: true, window: true } });
    if (url.pathname === "/api/sessions/manage") {
      const a = url.searchParams.get("agent") === "codex" ? "codex" : "claude";
      return json({
        agents, agent: a,
        home: "/Users/me",
        sessions: a === "codex" ? codexSessions : claudeSessions,
        trash: [],
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
      test(`${engine} ${lang} ${width}px: worktrees group under repo and bb workspaces under Draft sessions`, async (t) => {
        const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
        t.after(() => browser.close());
        const page = await (await browser.newContext({ viewport: { width, height: 680 }, reducedMotion: "reduce" })).newPage();
        page.setDefaultTimeout(8000);
        page.setDefaultNavigationTimeout(60000);
        const errors = [];
        page.on("pageerror", (e) => errors.push(e.message));
        await page.route("**/*", serve(lang));
        await page.goto("http://magpie.test/");
        await page.locator('#nav [data-view="sessions"]').click();

        const view = page.locator("#view-sessions");
        const folders = view.locator(".sm-folders > .sm-group");
        const tr = (k) => page.evaluate(([lang, k]) => (typeof I18N !== "undefined" && I18N[lang]?.[k]) || k, [lang, k]);
        const draftsName = await tr("Draft sessions");

        await folders.first().waitFor();
        assert.deepEqual(await folders.locator(".fold .name").allTextContents(), ["app", "lib", draftsName]);

        // Unfolded "app" holds main + both worktree sessions, and each worktree row keeps its own cwd as hover title.
        const appIds = await folders.nth(0).locator(".row.sm-sess").evaluateAll((els) => els.map((e) => e.dataset.id));
        assert.deepEqual(appIds, ["cc-main", "cc-bb-wt", "cc-cl-wt"]);
        assert.equal(await folders.nth(0).locator('.row.sm-sess[data-id="cc-bb-wt"]').getAttribute("title"), "/Users/me/.bb-machines/devbox/plugins/environment-git-worktree/host-data/worktrees/thr_wt1-0/app");
        assert.equal(await folders.nth(0).locator('.row.sm-sess[data-id="cc-main"]').getAttribute("title"), null);

        // "app" (has main session) offers Relocate project; "lib" (only linked worktree) only offers Delete all sessions.
        await folders.nth(0).locator(".sm-folder-more").click();
        await page.locator(".pop.row-menu").waitFor();
        assert.equal(await page.locator(".pop.row-menu .rm-item", { hasText: await tr("Relocate project") }).count(), 1);
        await page.keyboard.press("Escape");
        await page.locator(".pop.row-menu").waitFor({ state: "detached" });
        await folders.nth(1).locator(".sm-folder-more").click();
        await page.locator(".pop.row-menu").waitFor();
        assert.equal(await page.locator(".pop.row-menu .rm-item", { hasText: await tr("Relocate project") }).count(), 0);
        await page.keyboard.press("Escape");
        await page.locator(".pop.row-menu").waitFor({ state: "detached" });

        // Unfolding Draft sessions reveals both bb personal workspace and deleted bb worktree; only ~/.cache/scratch sits in Other folders.
        await folders.nth(2).locator(".fold").click();
        const draftIds = await folders.nth(2).locator(".row.sm-sess").evaluateAll((els) => els.map((e) => e.dataset.id));
        assert.deepEqual(draftIds, ["cc-bb-draft", "cc-bb-gone"]);
        const others = view.locator(".sm-others");
        assert.equal(await others.locator(".sm-others-fold .name").textContent(), await tr("Other folders"));
        await others.locator(".sm-others-fold").click();
        assert.deepEqual(await others.locator(".sm-group .fold .name").allTextContents(), ["scratch"]);

        // Switching to Codex: resolved ~/.codex/worktrees session groups under "app", NOT Draft sessions,
        // while bb personal workspace groups under Draft sessions, leaving no Other folders.
        await page.evaluate(() => magpie.call("sessions", "reveal", {
          agent: "codex", id: "cx-wt-resolved", cwd: "/Users/me/.codex/worktrees/ab12/app", worktree_of: "/Users/me/code/app",
        }));
        await view.locator('.sm-item.open .row.sm-sess[data-id="cx-wt-resolved"]').waitFor();
        assert.deepEqual(await folders.locator(".fold .name").allTextContents(), ["app", draftsName]);
        assert.equal(await others.count(), 0);

        assert.deepEqual(errors, []);
        await page.context().close();
      });
    }
  }
}
