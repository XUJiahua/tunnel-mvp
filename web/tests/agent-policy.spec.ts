import { test, expect } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import http from "node:http";
import { createHash } from "node:crypto";
const root = resolve(process.cwd(), ".."),
  url = "http://localhost:28880",
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
  dir = mkdtempSync(resolve(tmpdir(), "tunnel-policy-e2e-"));
  writeFileSync(
    resolve(dir, "server.json"),
    JSON.stringify({
      listen: "127.0.0.1:28880",
      publicURL: url,
      accessURL: "http://apps.localhost:28880",
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
    if (req.url === "/events") {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write("data: ready\n\n");
      const timer = setInterval(() => res.write(": heartbeat\n\n"), 300);
      req.on("close", () => clearInterval(timer));
      return;
    }
    if (req.url === "/upload") {
      let n = 0;
      req.on("data", (b) => (n += b.length));
      req.on("end", () => res.end(String(n)));
      return;
    }
    res.setHeader("Content-Type", "text/html");
    res.end("<h1>Redundant Agent application</h1>");
  });
  upstream.on("upgrade", (req, socket) => {
    const accept = createHash("sha1")
      .update(
        String(req.headers["sec-websocket-key"]) +
          "258EAFA5-E914-47DA-95CA-C5AB0DC85B11",
      )
      .digest("base64");
    socket.write(
      "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
        accept +
        "\r\n\r\n",
    );
    socket.write(Buffer.from([0x81, 5, ...Buffer.from("ready")]));
    socket.on("data", () => {});
    socket.on("end", () => socket.end());
    socket.on("error", () => {});
  });
  await new Promise<void>((done) => upstream.listen(28882, "127.0.0.1", done));
});
test.afterAll(async () => {
  for (const c of processes.reverse()) await stop(c);
  if (upstream) await new Promise<void>((done) => upstream.close(() => done()));
  if (dir) rmSync(dir, { recursive: true, force: true });
});
test("remote policy UI, initial deny, live stream narrowing and local authorization ceiling", async ({
  page,
  context,
}) => {
  await page.goto(url);
  await page.getByLabel("用户名", { exact: true }).fill("admin");
  await page.getByLabel("密码", { exact: true }).fill(password);
  await page.getByRole("button", { name: "登录控制台", exact: true }).click();
  await expect(page.locator(".profile")).toContainText("admin");
  async function api(path: string, method = "GET", data?: unknown) {
    const r = await page.request.fetch(url + "/api" + path, {
      method,
      headers: { Origin: url },
      data,
    });
    expect(r.ok(), `API ${method} ${path}: ${r.status()}`).toBe(true);
    return r.json();
  }
  const { site } = await api("/sites", "POST", { name: "Constrained site" });
  const app = await api("/services", "POST", {
    name: "Constrained app",
    siteID: site.id,
    url: "http://127.0.0.1:28882",
  });
  const registration = await api(`/sites/${site.id}/agents`, "POST", {
    name: "Constrained Agent",
  });
  const path = resolve(dir, "agent.json");
  writeFileSync(
    path,
    JSON.stringify({
      serverURL: url,
      siteID: site.id,
      instanceID: registration.agent.id,
      remotePolicy: true,
      allow: [{ cidr: "127.0.0.0/8", ports: [28882] }],
    }),
  );
  start(["agent", "-c", path], { TUNNEL_AGENT_TOKEN: registration.token });
  await expect.poll(async () => (await api("/agents"))[0].online).toBe(true);
  const launch = await api(`/services/${app.id}/launch`, "POST"),
    application = await context.newPage();
  await application.goto(launch.url);
  await expect(application.locator("body")).toContainText("acl_denied");
  await page.reload();
  await page.getByRole("button", { name: "Agent 实例", exact: true }).click();
  await page.getByRole("button", { name: "编辑收紧策略", exact: true }).click();
  await page
    .getByLabel("远程允许规则 JSON", { exact: true })
    .fill(JSON.stringify([{ cidr: "0.0.0.0/0", ports: [28882, 28883] }]));
  await page.getByRole("button", { name: "保存收紧策略", exact: true }).click();
  await expect
    .poll(
      async () => {
        const a = (await api("/agents"))[0];
        return a.appliedPolicyVersion === a.desiredPolicyVersion;
      },
      { timeout: 15000 },
    )
    .toBe(true);
  await application.reload();
  await expect(
    application.getByRole("heading", { name: "Redundant Agent application" }),
  ).toBeVisible();
  await application.evaluate(() => {
    const w = window as any;
    void (async () => {
      try {
        const r = await fetch("/events"),
          reader = r.body!.getReader();
        await reader.read();
        w.ready = true;
        while (!(await reader.read()).done) {}
      } catch {
      } finally {
        w.streamClosed = true;
      }
    })();
  });
  await expect
    .poll(() => application.evaluate(() => (window as any).ready))
    .toBe(true);
  await page.getByRole("button", { name: "编辑收紧策略", exact: true }).click();
  await page.getByLabel("远程允许规则 JSON", { exact: true }).fill("[]");
  await page.getByRole("button", { name: "保存收紧策略", exact: true }).click();
  await expect
    .poll(() => application.evaluate(() => (window as any).streamClosed), {
      timeout: 15000,
    })
    .toBe(true);
  await expect
    .poll(
      async () => {
        const a = (await api("/agents"))[0];
        return a.appliedPolicyVersion === a.desiredPolicyVersion;
      },
      { timeout: 15000 },
    )
    .toBe(true);
  await application.reload();
  await expect(application.locator("body")).toContainText("acl_denied");
  await page.getByRole("button", { name: "编辑收紧策略", exact: true }).click();
  await page
    .getByLabel("远程允许规则 JSON", { exact: true })
    .fill('[{"cidr":"invalid","ports":[80]}]');
  await page.getByRole("button", { name: "保存收紧策略", exact: true }).click();
  await expect(page.locator(".agent-panel").getByRole("alert")).toContainText("CIDR");
  const state = await api(
    `/sites/${site.id}/agents/${registration.agent.id}/policy`,
  );
  expect(state.allow).toEqual([]);
  await application.close();
});
