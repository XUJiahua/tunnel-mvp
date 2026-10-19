import { test, expect } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import http from "node:http";
const root = resolve(process.cwd(), ".."),
  url = "http://localhost:21880",
  password = "group-e2e-password-123";
let dir: string,
  server: ChildProcess,
  agent: ChildProcess,
  upstream: http.Server;
let appID: string, siblingID: string;
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
  dir = mkdtempSync(resolve(tmpdir(), "tunnel-group-e2e-"));
  writeFileSync(
    resolve(dir, "server.json"),
    JSON.stringify({
      listen: "127.0.0.1:21880",
      publicURL: url,
      accessURL: "http://apps.localhost:21880",
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
    res.end("<!doctype html><h1>Group authorized application</h1>"),
  );
  await new Promise<void>((done) => upstream.listen(21882, "127.0.0.1", done));
});
test.afterAll(async () => {
  await stop(agent);
  await stop(server);
  if (upstream) await new Promise<void>((done) => upstream.close(() => done()));
  if (dir) rmSync(dir, { recursive: true, force: true });
});
test("one-time invitation joins group, application boundary and live group revocation", async ({
  page,
  browser,
}) => {
  await page.goto(url);
  await page.getByLabel("用户名", { exact: true }).fill("admin");
  await page.getByLabel("密码", { exact: true }).fill(password);
  await page.getByRole("button", { name: "登录控制台", exact: true }).click();
  await expect(page.locator(".profile")).toContainText("admin");
  async function api(path: string, body: unknown) {
    const res = await page.request.post(url + "/api" + path, {
      headers: { Origin: url },
      data: body,
    });
    expect(res.ok()).toBe(true);
    return res.json();
  }
  const site = await api("/sites", { name: "group site" });
  const app = await api("/services", {
    name: "Group App",
    siteID: site.site.id,
    url: "http://127.0.0.1:21882",
  });
  appID = app.id;
  const sibling = await api("/services", {
    name: "Private App",
    siteID: site.site.id,
    url: "http://127.0.0.1:21882",
  });
  siblingID = sibling.id;
  writeFileSync(
    resolve(dir, "agent.json"),
    JSON.stringify({
      serverURL: url,
      siteID: site.site.id,
      allow: [{ cidr: "127.0.0.0/8", ports: [21882] }],
    }),
  );
  agent = start(["agent", "-c", resolve(dir, "agent.json")], {
    TUNNEL_AGENT_TOKEN: site.token,
  });
  await expect
    .poll(async () => {
      const res = await page.request.get(url + "/api/sites");
      const rows = await res.json();
      return rows[0]?.online;
    })
    .toBe(true);
  await page.reload();
  await page.getByRole("button", { name: "用户组与邀请", exact: true }).click();
  await page.getByRole("button", { name: "创建用户组", exact: true }).click();
  await page.getByLabel("用户组名称").fill("Operators");
  await page.getByLabel("Group App · group site", { exact: true }).check();
  await page.getByRole("button", { name: "保存", exact: true }).click();
  await expect(page.locator(".group-editor")).toHaveCount(0);
  await page.getByRole("button", { name: "邀请成员", exact: true }).click();
  await page.getByLabel("受邀用户名").fill("invited-member");
  await page.getByLabel("Operators", { exact: true }).check();
  await page.getByRole("button", { name: "保存", exact: true }).click();
  const link = await page.locator(".invite-link").textContent();
  expect(link).toContain("#invite=");
  const context = await browser.newContext();
  const member = await context.newPage();
  try {
    await member.goto(link!);
    await expect(
      member.getByRole("heading", { name: "接受成员邀请" }),
    ).toBeVisible();
    await expect(member).toHaveURL(url + "/");
    await member.getByLabel("设置密码", { exact: true }).fill(password);
    await member.getByLabel("确认密码", { exact: true }).fill(password);
    await member.getByRole("button", { name: "接受邀请", exact: true }).click();
    await expect(member.locator(".profile")).toContainText("invited-member");
    const apps = await member.request.get(url + "/api/services");
    expect((await apps.json()).map((v: { id: string }) => v.id)).toEqual([
      appID,
    ]);
    const siblingAttempt = await member.request.post(
      url + `/api/services/${siblingID}/launch`,
      { headers: { Origin: url } },
    );
    expect(siblingAttempt.status()).toBe(404);
    const popup = member.waitForEvent("popup");
    await member.getByRole("button", { name: "打开", exact: true }).click();
    const appPage = await popup;
    await expect(appPage.getByRole("heading")).toHaveText(
      "Group authorized application",
    );
    // Reopening the consumed link in the same document must also be rejected.
    await member.goto(link!);
    await expect(member.locator(".form-error")).toContainText(
      "invitation invalid or expired",
    );
    await member.getByRole("button", { name: "返回登录", exact: true }).click();
    await page.reload();
    await page
      .getByRole("button", { name: "用户组与邀请", exact: true })
      .click();
    const row = page
      .getByRole("row")
      .filter({ has: page.getByText("Operators", { exact: true }) });
    await row.getByRole("button", { name: "编辑用户组", exact: true }).click();
    await page.getByLabel("Group App · group site", { exact: true }).uncheck();
    await page.getByRole("button", { name: "保存", exact: true }).click();
    await expect(page.locator(".group-editor")).toHaveCount(0);
    await appPage.reload();
    await expect(appPage.locator("body")).not.toContainText(
      "Group authorized application",
    );
    expect((await member.request.get(url + "/api/me")).status()).toBe(200);
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
