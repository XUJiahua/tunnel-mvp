import { test, expect } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import http from "node:http";
import { createHash } from "node:crypto";
const root = resolve(process.cwd(), ".."),
  url = "http://localhost:30880",
  password = "agent-instance-e2e-password-123";
let dir: string, server: ChildProcess, upstream: http.Server;
const processes: ChildProcess[] = [];
function start(args: string[], env: Record<string, string> = {}) {
  const c = spawn(resolve(root, "bin/tunnel"), args, {
    env: { ...process.env, ...env },
    stdio: "ignore",
  });
  processes.push(c);
  return c;
}
async function stop(c?: ChildProcess) {
  if (c?.exitCode === null && c.signalCode === null)
    await new Promise<void>((done) => {
      c.once("exit", () => done());
      c.kill("SIGTERM");
    });
}
test.beforeAll(async () => {
  dir = mkdtempSync(resolve(tmpdir(), "tunnel-session-pools-e2e-"));
  writeFileSync(
    resolve(dir, "server.json"),
    JSON.stringify({
      listen: "127.0.0.1:30880",
      publicURL: url,
      accessURL: "http://apps.localhost:30880",
      dataDir: resolve(dir, "data"),
      storage: process.env.TUNNEL_TEST_STORAGE || "json",
    }),
  );
  server = start(["server", "-c", resolve(dir, "server.json")], {
    TUNNEL_ADMIN_PASSWORD: password,
  });
  await expect
    .poll(async () => {
      try {
        return (await fetch(url + "/healthz")).ok;
      } catch {
        return false;
      }
    })
    .toBe(true);
  const identities = new Map<object, string>();
  upstream = http.createServer((req, res) => {
    if (req.headers.authorization)
      identities.set(req.socket, req.headers.authorization);
    const identity = identities.get(req.socket) || "anonymous";
    res.setHeader("Content-Type", "text/html");
    res.end("<h1>" + identity + "</h1>");
  });
  upstream.on("connection", (socket) =>
    socket.on("close", () => identities.delete(socket)),
  );
  await new Promise<void>((done) => upstream.listen(30882, "127.0.0.1", done));
});
test.afterAll(async () => {
  for (const c of processes.reverse()) await stop(c);
  if (upstream) await new Promise<void>((done) => upstream.close(() => done()));
  if (dir) rmSync(dir, { recursive: true, force: true });
});
test("session pool UI, browser identity isolation and resource mode withdrawal", async ({
  page,
  browser,
}) => {
  async function login(p: typeof page, username: string) {
    await p.goto(url);
    await p.getByLabel("用户名", { exact: true }).fill(username);
    await p.getByLabel("密码", { exact: true }).fill(password);
    await p.getByRole("button", { name: "登录控制台", exact: true }).click();
    await expect(p.locator(".profile")).toContainText(username);
  }
  await login(page, "admin");
  async function api(path: string, method = "GET", data?: unknown) {
    const r = await page.request.fetch(url + "/api" + path, {
      method,
      headers: { Origin: url },
      data,
    });
    expect(r.ok(), `API ${method} ${path}: ${r.status()}`).toBe(true);
    return r.json();
  }
  const { site, token } = await api("/sites", "POST", {
      name: "Identity-bound site",
    }),
    app = await api("/services", "POST", {
      name: "Identity-bound application",
      siteID: site.id,
      url: "http://127.0.0.1:30882",
    });
  const path = resolve(dir, "agent.json");
  writeFileSync(
    path,
    JSON.stringify({
      serverURL: url,
      siteID: site.id,
      allow: [{ cidr: "127.0.0.0/8", ports: [30882] }],
    }),
  );
  start(["agent", "-c", path], { TUNNEL_AGENT_TOKEN: token });
  await expect.poll(async () => (await api("/sites"))[0].online).toBe(true);
  await page.reload();
  await page.locator("nav").getByRole("button", { name: /^应用/ }).click();
  await page
    .getByRole("row")
    .filter({ hasText: "Identity-bound application" })
    .getByRole("button", { name: "编辑", exact: true })
    .click();
  await page
    .getByLabel("上游连接模式", { exact: true })
    .selectOption("session");
  await page.getByRole("button", { name: "保存", exact: true }).click();
  await expect
    .poll(async () => (await api("/services"))[0].connectionMode)
    .toBe("session");
  await api("/users", "POST", {
    username: "bound-viewer",
    password,
    serviceIDs: [app.id],
  });
  const viewerContext = await browser.newContext(),
    viewer = await viewerContext.newPage();
  await login(viewer, "bound-viewer");
  const ownerLaunch = await api(`/services/${app.id}/launch`, "POST"),
    ownerApp = await page.context().newPage();
  await ownerApp.goto(ownerLaunch.url);
  const viewerLaunch = await (
      await viewer.request.post(url + `/api/services/${app.id}/launch`, {
        headers: { Origin: url },
      })
    ).json(),
    viewerApp = await viewerContext.newPage();
  await viewerApp.goto(viewerLaunch.url);
  await expect(
    viewerApp.getByRole("heading", { name: "anonymous", exact: true }),
  ).toBeVisible();
  expect(
    await ownerApp.evaluate(async () =>
      (
        await fetch("/who", { headers: { Authorization: "Mock Alice" } })
      ).text(),
    ),
  ).toContain("Mock Alice");
  expect(
    await viewerApp.evaluate(async () => (await fetch("/who")).text()),
  ).toContain("anonymous");
  expect(
    await viewerApp.evaluate(async () =>
      (await fetch("/who", { headers: { Authorization: "Mock Bob" } })).text(),
    ),
  ).toContain("Mock Bob");
  expect(
    await ownerApp.evaluate(async () => (await fetch("/who")).text()),
  ).toContain("Mock Alice");
  expect(
    await viewerApp.evaluate(async () => (await fetch("/who")).text()),
  ).toContain("Mock Bob");
  await api(`/services/${app.id}/probe`, "POST");
  expect(
    await ownerApp.evaluate(async () => (await fetch("/who")).text()),
  ).toContain("Mock Alice");
  await page
    .getByRole("row")
    .filter({ hasText: "Identity-bound application" })
    .getByRole("button", { name: "编辑", exact: true })
    .click();
  await page.getByLabel("上游连接模式", { exact: true }).selectOption("");
  await page.getByRole("button", { name: "保存", exact: true }).click();
  await expect
    .poll(() => viewerApp.evaluate(async () => (await fetch("/who")).status))
    .toBe(401);
  await viewerContext.close();
  await ownerApp.close();
});
