import { test, expect } from "@playwright/test";
import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import {
  mkdtempSync,
  writeFileSync,
  rmSync,
  readFileSync,
  existsSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import http from "node:http";
const root = resolve(process.cwd(), ".."),
  url = "http://localhost:25880",
  password = "monitor-e2e-password-123";
let dir: string,
  server: ChildProcess,
  agent: ChildProcess,
  upstream: http.Server;
let healthy = true;
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
  dir = mkdtempSync(resolve(tmpdir(), "tunnel-monitor-e2e-"));
  writeFileSync(
    resolve(dir, "server.json"),
    JSON.stringify({
      listen: "127.0.0.1:25880",
      publicURL: url,
      accessURL: "http://apps.localhost:25880",
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
  upstream = http.createServer((req, res) => {
    if (req.url === "/business/ready" && req.method === "GET") {
      res.writeHead(healthy ? 204 : 503);
      res.end();
      return;
    }
    res.writeHead(401);
    res.end("Application login required");
  });
  await new Promise<void>((done) => upstream.listen(25882, "127.0.0.1", done));
});
test.afterAll(async () => {
  await stop(agent);
  await stop(server);
  if (upstream) await new Promise<void>((done) => upstream.close(() => done()));
  if (dir) rmSync(dir, { recursive: true, force: true });
});

test("business health history survives verified offline archive and cross-backend restore", async ({
  page,
  browser,
}) => {
  async function login(p: typeof page, user = "admin") {
    await p.goto(url);
    await p.getByLabel("用户名", { exact: true }).fill(user);
    await p.getByLabel("密码", { exact: true }).fill(password);
    await p.getByRole("button", { name: "登录控制台", exact: true }).click();
    await expect(p.locator(".profile")).toContainText(user);
  }
  await login(page);
  async function api(path: string, body: unknown) {
    const r = await page.request.post(url + "/api" + path, {
      headers: { Origin: url },
      data: body,
    });
    expect(r.ok()).toBe(true);
    return r.json();
  }
  const site = await api("/sites", { name: "Monitor site" }),
    app = await api("/services", {
      name: "Business application",
      siteID: site.site.id,
      url: "http://127.0.0.1:25882/business/",
    }),
    other = await api("/services", {
      name: "Private application",
      siteID: site.site.id,
      url: "http://127.0.0.1:25882",
    });
  await api("/users", { username: "viewer", password, serviceIDs: [app.id] });
  const agentPath = resolve(dir, "agent.json");
  writeFileSync(
    agentPath,
    JSON.stringify({
      serverURL: url,
      siteID: site.site.id,
      allow: [{ cidr: "127.0.0.0/8", ports: [25882] }],
    }),
  );
  agent = start(["agent", "-c", agentPath], { TUNNEL_AGENT_TOKEN: site.token });
  async function online() {
    await expect
      .poll(async () => {
        const r = await page.request.get(url + "/api/sites");
        return (await r.json())[0]?.online;
      })
      .toBe(true);
  }
  await online();
  await page.getByRole("button", { name: /^应用/ }).click();
  await page
    .getByRole("row")
    .filter({ has: page.getByText("Business application", { exact: true }) })
    .getByRole("button", { name: "编辑", exact: true })
    .click();
  const editor = page.getByRole("dialog");
  await editor.getByLabel("健康检查相对路径", { exact: true }).fill("ready");
  await editor.getByLabel("健康检查方法", { exact: true }).selectOption("GET");
  await editor.getByLabel("健康预期状态", { exact: true }).fill("204");
  await editor.getByRole("button", { name: "保存", exact: true }).click();
  await expect(editor).toHaveCount(0);
  await page.getByRole("button", { name: "监控历史", exact: true }).click();
  await page.getByLabel("监控应用", { exact: true }).selectOption(app.id);
  await page.getByRole("button", { name: "检测并刷新", exact: true }).click();
  await expect(
    page.getByRole("status").filter({ hasText: "1 次检查" }),
  ).toContainText("1 次健康");
  await expect(
    page.getByRole("img", { name: "应用健康状态与响应延迟历史" }),
  ).toBeVisible();
  healthy = false;
  await page.getByRole("button", { name: "检测并刷新", exact: true }).click();
  await expect(
    page.getByRole("status").filter({ hasText: "2 次检查" }),
  ).toContainText("1 次健康");
  await expect(
    page.getByRole("cell", { name: "503", exact: true }),
  ).toBeVisible();
  const context = await browser.newContext(),
    viewer = await context.newPage();
  try {
    await login(viewer, "viewer");
    await viewer.getByRole("button", { name: "监控历史", exact: true }).click();
    await expect(
      viewer.getByRole("status").filter({ hasText: "2 次检查" }),
    ).toBeVisible();
    expect(
      (
        await viewer.request.get(url + `/api/services/${other.id}/history`)
      ).status(),
    ).toBe(404);
    const binary = resolve(root, "bin/tunnel"),
      archive = resolve(dir, "operations.tar.gz"),
      snapshot = resolve(dir, "state-backup.json");
    function cli(args: string[], ok = true) {
      const r = spawnSync(binary, args, { encoding: "utf8", env: process.env });
      expect(r.status === 0, r.stderr).toBe(ok);
      expect(r.stdout + r.stderr).not.toContain(password);
      return r;
    }
    const originalConfig = resolve(dir, "server.json");
    const locked = cli(
      [
        "storage",
        "export-operations",
        "-c",
        originalConfig,
        "--output",
        archive,
      ],
      false,
    );
    expect(locked.stderr).toContain("data directory is in use");
    expect(existsSync(archive)).toBe(false);
    await stop(agent);
    await stop(server);
    cli(["storage", "export", "-c", originalConfig, "--output", snapshot]);
    cli([
      "storage",
      "export-operations",
      "-c",
      originalConfig,
      "--output",
      archive,
    ]);
    cli(["storage", "validate-operations", "--input", archive]);
    const bytes = readFileSync(archive),
      bad = resolve(dir, "truncated.tar.gz");
    writeFileSync(bad, bytes.subarray(0, bytes.length - 8));
    cli(["storage", "validate-operations", "--input", bad], false);
    const restoredConfig = resolve(dir, "restored.json"),
      restored = resolve(dir, "restored"),
      targetStorage =
        process.env.TUNNEL_TEST_STORAGE === "sqlite" ? "json" : "sqlite";
    writeFileSync(
      restoredConfig,
      JSON.stringify({
        listen: "127.0.0.1:25880",
        publicURL: url,
        accessURL: "http://apps.localhost:25880",
        dataDir: restored,
        storage: targetStorage,
      }),
    );
    cli(["storage", "restore", "-c", restoredConfig, "--input", snapshot]);
    cli([
      "storage",
      "restore-operations",
      "-c",
      restoredConfig,
      "--input",
      archive,
    ]);
    cli(
      [
        "storage",
        "restore-operations",
        "-c",
        restoredConfig,
        "--input",
        archive,
      ],
      false,
    );
    server = start(["server", "-c", restoredConfig], {
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
    await login(page);
    agent = start(["agent", "-c", agentPath], {
      TUNNEL_AGENT_TOKEN: site.token,
    });
    await online();
    await page.getByRole("button", { name: "监控历史", exact: true }).click();
    await page.getByLabel("监控应用", { exact: true }).selectOption(app.id);
    await expect(
      page.getByRole("status").filter({ hasText: "2 次检查" }),
    ).toContainText("1 次健康");
    const audit = await (
      await page.request.get(url + "/api/audit?event=service_updated")
    ).json();
    expect(audit.entries.length).toBeGreaterThan(0);
    healthy = true;
    await page.getByRole("button", { name: "检测并刷新", exact: true }).click();
    await expect(
      page.getByRole("status").filter({ hasText: "3 次检查" }),
    ).toContainText("2 次健康");
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
