import { test, expect, type Page } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import {
  mkdtempSync,
  writeFileSync,
  rmSync,
  mkdirSync,
  existsSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
const root = resolve(process.cwd(), ".."),
  url = "http://localhost:26880",
  password = "audit-e2e-password-123";
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
  dir = mkdtempSync(resolve(tmpdir(), "tunnel-audit-e2e-"));
  mkdirSync(resolve(dir, "data"), { recursive: true });
  const events =
    Array.from({ length: 250 }, (_, i) =>
      JSON.stringify({
        time: "2024-01-01T00:00:00Z",
        event: "historical_event",
        user: "retired-operator",
        siteID: "retired-site",
        resourceID: `old-app-${i}`,
        padding: "x".repeat(10000),
      }),
    ).join("\n") + "\n";
  writeFileSync(resolve(dir, "data/audit.jsonl.1"), events);
  writeFileSync(
    resolve(dir, "data/audit.jsonl"),
    (
      JSON.stringify({
        event: "rotation_seed",
        padding: "x".repeat(64 << 10),
      }) + "\n"
    ).repeat(17),
  );
  writeFileSync(
    resolve(dir, "server.json"),
    JSON.stringify({
      auditMaxMB: 1,
      listen: "127.0.0.1:26880",
      publicURL: url,
      accessURL: "http://apps.localhost:26880",
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

test("retained audit search, pagination, scoped privacy and restart cursor invalidation", async ({
  page,
  browser,
}) => {
  await login(page, "admin", password);
  expect(existsSync(resolve(dir, "data/audit.jsonl.2"))).toBe(true);
  await page.getByRole("button", { name: "审计日志", exact: true }).click();
  await page.getByLabel("事件类型", { exact: true }).fill("historical_event");
  await page.getByLabel("操作者", { exact: true }).fill("retired-operator");
  await page.getByRole("button", { name: "查询审计", exact: true }).click();
  const rows = page
    .getByRole("row")
    .filter({ has: page.getByText("historical_event", { exact: true }) });
  await expect(rows).toHaveCount(100);
  await expect(page.getByText("old-app-249", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "加载更早记录", exact: true }).click();
  await expect(rows).toHaveCount(200);
  await page.getByRole("button", { name: "加载更早记录", exact: true }).click();
  await expect(rows).toHaveCount(250);
  await expect(
    page.getByRole("button", { name: "加载更早记录", exact: true }),
  ).toHaveCount(0);
  await page.getByLabel("资源 ID", { exact: true }).fill("old-app-137");
  await page.getByRole("button", { name: "查询审计", exact: true }).click();
  await expect(rows).toHaveCount(1);
  await expect(rows).toContainText("old-app-137");
  const first = await (
    await page.request.get(url + "/api/audit?event=historical_event&limit=2")
  ).json();
  expect(first.nextCursor).toBeTruthy();
  async function create(path: string, body: unknown) {
    const r = await page.request.post(url + "/api" + path, {
      headers: { Origin: url },
      data: body,
    });
    expect(r.status()).toBe(201);
    return r.json();
  }
  const member = await create("/users", { username: "auditor", password }),
    role = await create("/roles", {
      name: "Default auditor",
      permissions: ["audit:read"],
    });
  const bound = await page.request.patch(url + `/api/users/${member.id}/role`, {
    headers: { Origin: url },
    data: { roleID: role.id },
  });
  expect(bound.status()).toBe(200);
  const context = await browser.newContext(),
    auditor = await context.newPage();
  try {
    await login(auditor, "auditor", password);
    await auditor
      .getByRole("button", { name: "审计日志", exact: true })
      .click();
    await auditor
      .getByLabel("事件类型", { exact: true })
      .fill("historical_event");
    await auditor
      .getByRole("button", { name: "查询审计", exact: true })
      .click();
    await expect(
      auditor.getByText("没有匹配的审计记录。", { exact: true }),
    ).toBeVisible();
    const scoped = await (await auditor.request.get(url + "/api/audit")).json();
    expect(scoped.indexedFiles).toBeUndefined();
    expect(
      scoped.entries.some(
        (e: { event: string }) => e.event === "role_assigned",
      ),
    ).toBe(true);
  } finally {
    await context.close();
  }
  await stop();
  start();
  await ready();
  await login(page, "admin", password);
  expect(
    (
      await page.request.get(
        url +
          "/api/audit?event=historical_event&limit=2&cursor=" +
          first.nextCursor,
      )
    ).status(),
  ).toBe(409);
  await page.getByRole("button", { name: "审计日志", exact: true }).click();
  await page.getByLabel("事件类型", { exact: true }).fill("historical_event");
  await page.getByRole("button", { name: "查询审计", exact: true }).click();
  await expect(
    page
      .getByRole("row")
      .filter({ has: page.getByText("historical_event", { exact: true }) }),
  ).toHaveCount(100);
  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
});
