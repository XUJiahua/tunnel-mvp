import { test, expect, type Page } from "@playwright/test";
import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
const root = resolve(process.cwd(), ".."),
  url = "http://localhost:20880",
  password = "session-e2e-password-123";
let dir: string, server: ChildProcess;
function start() {
  server = spawn(
    resolve(root, "bin/tunnel"),
    ["server", "-c", resolve(dir, "server.json")],
    {
      env: { ...process.env, TUNNEL_ADMIN_PASSWORD: password },
      stdio: "ignore",
    },
  );
}
async function stop() {
  if (server?.exitCode === null && server.signalCode === null)
    await new Promise<void>((done) => {
      server.once("exit", () => done());
      server.kill("SIGTERM");
    });
}
async function ready() {
  await expect
    .poll(async () => {
      try {
        return (await fetch(url + "/healthz")).ok;
      } catch {
        return false;
      }
    })
    .toBe(true);
}
async function login(page: Page, user: string, secret: string) {
  await page.goto(url);
  await page.getByLabel("用户名", { exact: true }).fill(user);
  await page.getByLabel("密码", { exact: true }).fill(secret);
  await page.getByRole("button", { name: "登录控制台", exact: true }).click();
  await expect(page.locator(".profile")).toContainText(user);
}
test.beforeAll(async () => {
  dir = mkdtempSync(resolve(tmpdir(), "tunnel-sessions-e2e-"));
  writeFileSync(
    resolve(dir, "server.json"),
    JSON.stringify({
      listen: "127.0.0.1:20880",
      publicURL: url,
      accessURL: "http://apps.localhost:20880",
      dataDir: resolve(dir, "data"),
      storage: process.env.TUNNEL_TEST_STORAGE || "json",
    }),
  );
  start();
  await ready();
});
test.afterAll(async () => {
  await stop();
  if (dir) rmSync(dir, { recursive: true, force: true });
});
test("device ownership, per-session admin revoke and offline administrator recovery", async ({
  page,
  browser,
}) => {
  await login(page, "admin", password);
  const created = await page.request.post(url + "/api/users", {
    headers: { Origin: url },
    data: { username: "member", password },
  });
  expect(created.status()).toBe(201);
  const firstContext = await browser.newContext(),
    secondContext = await browser.newContext();
  const first = await firstContext.newPage(),
    second = await secondContext.newPage();
  try {
    await login(first, "member", password);
    await login(second, "member", password);
    await page.getByRole("button", { name: "登录设备", exact: true }).click();
    await page.getByLabel("会话范围").selectOption("all");
    const rows = page
      .getByRole("row")
      .filter({ has: page.getByText("member", { exact: true }) });
    await expect(rows).toHaveCount(2);
    page.once("dialog", (dialog) => dialog.accept());
    await rows.first().getByRole("button", { name: "撤销会话" }).click();
    await expect(rows).toHaveCount(1);
    expect((await second.request.get(url + "/api/me")).status()).toBe(401);
    expect((await first.request.get(url + "/api/me")).status()).toBe(200);
    await first.getByRole("button", { name: "登录设备", exact: true }).click();
    await expect(first.getByLabel("会话范围")).toHaveCount(0);
    first.once("dialog", (dialog) => dialog.accept());
    await first.getByRole("button", { name: "撤销会话" }).click();
    await expect(
      first.getByRole("button", { name: "登录控制台", exact: true }),
    ).toBeVisible();
    const recoveryPassword = "offline-recovered-password-123";
    const args = [
      "storage",
      "reset-admin-password",
      "-c",
      resolve(dir, "server.json"),
      "--username",
      "admin",
    ];
    const env = { ...process.env, TUNNEL_RECOVERY_PASSWORD: recoveryPassword };
    const locked = spawnSync(resolve(root, "bin/tunnel"), args, {
      env,
      encoding: "utf8",
    });
    expect(locked.status).not.toBe(0);
    expect(locked.stdout + locked.stderr).toContain("data directory is in use");
    await stop();
    const recovered = spawnSync(resolve(root, "bin/tunnel"), args, {
      env,
      encoding: "utf8",
    });
    expect(recovered.status, recovered.stderr).toBe(0);
    expect(recovered.stdout + recovered.stderr).not.toContain(recoveryPassword);
    start();
    await ready();
    await page.goto(url);
    await expect(
      page.getByRole("button", { name: "登录控制台", exact: true }),
    ).toBeVisible();
    await login(page, "admin", recoveryPassword);
    expect((await page.request.get(url + "/api/users")).status()).toBe(200);
    const old = await page.request.post(url + "/api/login", {
      headers: { Origin: url },
      data: { username: "admin", password },
    });
    expect(old.status()).toBe(401);
  } finally {
    await firstContext.close();
    await secondContext.close();
  }
});
