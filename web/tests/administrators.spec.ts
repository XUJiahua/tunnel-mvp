import { test, expect } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import http from "node:http";
const root = resolve(process.cwd(), ".."),
  url = "http://localhost:22880",
  password = "admin-e2e-password-123";
let dir: string,
  server: ChildProcess,
  agent: ChildProcess,
  upstream: http.Server;

function start(args: string[], env: Record<string, string>) {
  return spawn(resolve(root, "bin/tunnel"), args, {
    env: { ...process.env, ...env },
    stdio: "ignore",
  });
}
async function stop(child?: ChildProcess) {
  if (child?.exitCode === null && child.signalCode === null)
    await new Promise<void>((done) => {
      child.once("exit", () => done());
      child.kill("SIGTERM");
    });
}
test.beforeAll(async () => {
  dir = mkdtempSync(resolve(tmpdir(), "tunnel-admin-e2e-"));
  writeFileSync(
    resolve(dir, "server.json"),
    JSON.stringify({
      listen: "127.0.0.1:22880",
      publicURL: url,
      accessURL: "http://apps.localhost:22880",
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
  upstream = http.createServer((_req, res) =>
    res.end("<!doctype html><h1>Administrator application</h1>"),
  );
  await new Promise<void>((done) => upstream.listen(22882, "127.0.0.1", done));
});
test.afterAll(async () => {
  await stop(agent);
  await stop(server);
  if (upstream) await new Promise<void>((done) => upstream.close(() => done()));
  if (dir) rmSync(dir, { recursive: true, force: true });
});

test("administrator password proof, last recovery account and role revocation", async ({
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
  async function api(path: string, body: unknown) {
    const r = await page.request.post(url + "/api" + path, {
      headers: { Origin: url },
      data: body,
    });
    expect(r.ok()).toBe(true);
    return r.json();
  }
  const site = await api("/sites", { name: "Administrator site" });
  await api("/services", {
    name: "Administrator app",
    siteID: site.site.id,
    url: "http://127.0.0.1:22882",
  });
  writeFileSync(
    resolve(dir, "agent.json"),
    JSON.stringify({
      serverURL: url,
      siteID: site.site.id,
      allow: [{ cidr: "127.0.0.0/8", ports: [22882] }],
    }),
  );
  agent = start(["agent", "-c", resolve(dir, "agent.json")], {
    TUNNEL_AGENT_TOKEN: site.token,
  });
  await expect
    .poll(async () => {
      const r = await page.request.get(url + "/api/sites");
      return (await r.json())[0]?.online;
    })
    .toBe(true);
  await page.getByRole("button", { name: "管理员", exact: true }).click();
  const rootRow = page
    .getByRole("row")
    .filter({ has: page.getByText("admin", { exact: true }) });
  await page.getByLabel("当前管理员密码", { exact: true }).fill(password);
  page.once("dialog", (d) => d.accept());
  await rootRow.getByRole("button", { name: "停用管理员" }).click();
  await expect(page.locator(".form-error")).toContainText("at least one");
  await page.getByLabel("当前管理员密码", { exact: true }).fill(password);
  await page.getByLabel("新管理员用户名", { exact: true }).fill("second-admin");
  await page.getByLabel("新管理员初始密码", { exact: true }).fill(password);
  await page.getByRole("button", { name: "创建管理员", exact: true }).click();
  const secondRow = page
    .getByRole("row")
    .filter({ has: page.getByText("second-admin", { exact: true }) });
  await expect(secondRow).toContainText("全局管理员");
  const context = await browser.newContext(),
    second = await context.newPage();
  try {
    await login(second, "second-admin");
    expect((await second.request.get(url + "/api/users")).status()).toBe(200);
    await second.getByRole("button", { name: /^应用/ }).click();
    const popup = second.waitForEvent("popup");
    await second.getByRole("button", { name: "打开", exact: true }).click();
    const app = await popup;
    await expect(app.getByRole("heading")).toHaveText(
      "Administrator application",
    );
    await page.getByLabel("当前管理员密码", { exact: true }).fill(password);
    page.once("dialog", (d) => d.accept());
    await secondRow
      .getByRole("button", { name: "降为成员", exact: true })
      .click();
    await expect(secondRow).toContainText("成员");
    expect((await second.request.get(url + "/api/me")).status()).toBe(401);
    await app.reload();
    await expect(app.locator("body")).not.toContainText(
      "Administrator application",
    );
    await login(second, "second-admin");
    expect((await second.request.get(url + "/api/users")).status()).toBe(403);
    await page
      .getByLabel("当前管理员密码", { exact: true })
      .fill("wrong-password");
    page.once("dialog", (d) => d.accept());
    await secondRow.getByRole("button", { name: "提升为管理员" }).click();
    await expect(page.locator(".form-error")).toContainText("current password");
    await page.getByLabel("当前管理员密码", { exact: true }).fill(password);
    page.once("dialog", (d) => d.accept());
    await secondRow.getByRole("button", { name: "提升为管理员" }).click();
    await expect(secondRow).toContainText("全局管理员");
    expect((await second.request.get(url + "/api/me")).status()).toBe(401);
    await login(second, "second-admin");
    await page.getByLabel("当前管理员密码", { exact: true }).fill(password);
    page.once("dialog", (d) => d.accept());
    await secondRow.getByRole("button", { name: "删除管理员" }).click();
    await expect(secondRow).toHaveCount(0);
    expect((await second.request.get(url + "/api/me")).status()).toBe(401);
    const old = await second.request.post(url + "/api/login", {
      headers: { Origin: url },
      data: { username: "second-admin", password },
    });
    expect(old.status()).toBe(401);
    await page.setViewportSize({ width: 390, height: 844 });
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
  } finally {
    await context.close();
  }
});
