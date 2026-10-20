import { test, expect } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";

// Avoid collisions with outbound sockets in the runner ephemeral port range.
const root = resolve(process.cwd(), ".."), url = "http://localhost:32080";
const password = "directory-e2e-password-123";
let dir: string, server: ChildProcess;
test.beforeAll(async () => {
  dir = mkdtempSync(resolve(tmpdir(), "tunnel-directory-e2e-"));
  writeFileSync(resolve(dir, "server.json"), JSON.stringify({
    listen: "127.0.0.1:32080", publicURL: url, accessURL: "http://apps.localhost:32080",
    dataDir: resolve(dir, "data"), storage: process.env.TUNNEL_TEST_STORAGE || "json",
  }));
  server = spawn(resolve(root, "bin/tunnel"), ["server", "-c", resolve(dir, "server.json")], {
    env: { ...process.env, TUNNEL_ADMIN_PASSWORD: password }, stdio: "ignore",
  });
  await expect.poll(async () => { try { return (await fetch(url + "/healthz")).ok; } catch { return false; } }).toBe(true);
});
test.afterAll(async () => {
  if (server?.exitCode === null && server.signalCode === null)
    await new Promise<void>((done) => { server.once("exit", () => done()); server.kill("SIGTERM"); });
  if (dir) rmSync(dir, { recursive: true, force: true });
});
test("directory UI loads beyond 100 and restarts after a real revision conflict", async ({ page }) => {
  const auth = await page.request.post(url + "/api/login", { headers: { Origin: url }, data: { username: "admin", password } });
  expect(auth.ok()).toBe(true);
  for (let i = 0; i < 103; i++) {
    const res = await page.request.post(url + "/api/sites", { headers: { Origin: url }, data: { name: `Directory site ${String(i).padStart(3, "0")}` } });
    expect(res.status()).toBe(201);
  }
  let firstPages = 0, conflicts = 0, injected = false;
  await page.route("**/api/sites?*", async (route) => {
    const requestURL = new URL(route.request().url());
    expect(requestURL.searchParams.get("limit")).toBe("100");
    if (!requestURL.searchParams.has("cursor")) firstPages++;
    else if (!injected) {
      injected = true;
      const mutation = await page.request.post(url + "/api/sites", { headers: { Origin: url }, data: { name: "Added between pages" } });
      expect(mutation.status()).toBe(201);
    }
    const response = await route.fetch();
    if (response.status() === 409) conflicts++;
    if (response.ok()) expect((await response.json()).length).toBeLessThanOrEqual(100);
    await route.fulfill({ response });
  });
  await page.goto(url);
  await expect(page.locator(".profile")).toContainText("admin");
  await page.locator("nav").getByRole("button", { name: /^站点/ }).click();
  await expect(page.locator(".filterbar")).toContainText("104 个站点");
  await page.getByRole("textbox", { name: "搜索", exact: true }).fill("Directory site 102");
  await expect(page.getByRole("heading", { name: "Directory site 102", exact: true })).toBeVisible();
  expect(conflicts).toBe(1);
  expect(firstPages).toBeGreaterThanOrEqual(2);
});
