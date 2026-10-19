import { test, expect } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import http from "node:http";
import { createHash } from "node:crypto";
const root = resolve(process.cwd(), ".."),
  url = "http://localhost:27880",
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
  dir = mkdtempSync(resolve(tmpdir(), "tunnel-agents-e2e-"));
  writeFileSync(
    resolve(dir, "server.json"),
    JSON.stringify({
      listen: "127.0.0.1:27880",
      publicURL: url,
      accessURL: "http://apps.localhost:27880",
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
  await new Promise<void>((done) => upstream.listen(27882, "127.0.0.1", done));
});
test.afterAll(async () => {
  for (const c of processes.reverse()) await stop(c);
  if (upstream) await new Promise<void>((done) => upstream.close(() => done()));
  if (dir) rmSync(dir, { recursive: true, force: true });
});
test("two real Agents, UI enrollment, stream ownership, process loss and individual credential revocation", async ({
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
  const { site } = await api("/sites", "POST", { name: "Redundant site" });
  const app = await api("/services", "POST", {
    name: "Redundant application",
    siteID: site.id,
    url: "http://127.0.0.1:27882",
  });
  await page.reload();
  await page.getByRole("button", { name: "Agent 实例", exact: true }).click();
  await page.getByLabel("实例名称", { exact: true }).fill("Instance A");
  await page.getByRole("button", { name: "登记 Agent", exact: true }).click();
  await expect(page.locator("pre")).toBeVisible();
  const credential = JSON.parse((await page.locator("pre").textContent())!);
  await page.getByRole("button", { name: "已保存，隐藏凭据" }).click();
  await expect(page.locator("pre")).toHaveCount(0);
  const second = await api(`/sites/${site.id}/agents`, "POST", {
    name: "Instance B",
  });
  const creds = [
    { id: credential.instanceID, token: credential.token },
    { id: second.agent.id, token: second.token },
  ];
  const agents = new Map<string, ChildProcess>();
  function run(c: { id: string; token: string }) {
    const path = resolve(dir, c.id + ".json");
    writeFileSync(
      path,
      JSON.stringify({
        serverURL: url,
        siteID: site.id,
        instanceID: c.id,
        allow: [{ cidr: "127.0.0.0/8", ports: [27882] }],
      }),
    );
    const proc = start(["agent", "-c", path], { TUNNEL_AGENT_TOKEN: c.token });
    agents.set(c.id, proc);
    return proc;
  }
  for (const c of creds) run(c);
  await expect
    .poll(
      async () =>
        (await api("/agents")).filter((a: { online: boolean }) => a.online)
          .length,
    )
    .toBe(2);
  await page
    .getByRole("button", { name: "禁用站点令牌兼容连接", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "启用站点令牌兼容连接", exact: true }),
  ).toBeVisible();
  const launch = await api(`/services/${app.id}/launch`, "POST"),
    application = await context.newPage();
  await application.goto(launch.url);
  await expect(
    application.getByRole("heading", { name: "Redundant Agent application" }),
  ).toBeVisible();
  expect(
    await application.evaluate(async () =>
      (
        await fetch("/upload", {
          method: "POST",
          body: new Uint8Array(1024 * 1024),
        })
      ).text(),
    ),
  ).toBe(String(1024 * 1024));
  await application.evaluate(() => {
    const w = window as typeof window & {
      streamReady?: boolean;
      streamClosed?: boolean;
    };
    void (async () => {
      try {
        const r = await fetch("/events");
        const reader = r.body!.getReader();
        await reader.read();
        w.streamReady = true;
        while (!(await reader.read()).done) {}
      } catch {
      } finally {
        w.streamClosed = true;
      }
    })();
  });
  await expect
    .poll(() => application.evaluate(() => (window as any).streamReady))
    .toBe(true);
  // New TCP connections alternate; pin a WebSocket to the other process as well.
  await application.evaluate(() => {
    const w = window as any;
    w.socket = new WebSocket(location.origin.replace("http", "ws") + "/ws");
    w.socket.onmessage = () => (w.wsReady = true);
    w.socket.onclose = () => (w.wsClosed = true);
  });
  await expect
    .poll(() => application.evaluate(() => (window as any).wsReady))
    .toBe(true);
  await expect
    .poll(
      async () =>
        (await api("/agents")).filter(
          (a: { activeChannels: number }) => a.activeChannels > 0,
        ).length,
    )
    .toBe(2);
  const inventory = await api("/agents");
  const victim = inventory[0].id;
  await stop(agents.get(victim));
  await expect
    .poll(
      async () =>
        (await api("/agents")).filter((a: { online: boolean }) => a.online)
          .length,
    )
    .toBe(1);
  await expect
    .poll(() =>
      application.evaluate(() =>
        Boolean((window as any).streamClosed || (window as any).wsClosed),
      ),
    )
    .toBe(true);
  expect(
    await application.evaluate(async () => (await fetch("/")).status),
  ).toBe(200);
  const survivor = creds.find((c) => c.id !== victim)!;
  await page
    .getByRole("row")
    .filter({ hasText: survivor.id })
    .getByRole("button", { name: "禁用", exact: true })
    .click();
  await expect
    .poll(
      async () =>
        (await api("/agents")).filter((a: { online: boolean }) => a.online)
          .length,
    )
    .toBe(0);
  await expect
    .poll(() =>
      application.evaluate(() =>
        Boolean((window as any).streamClosed && (window as any).wsClosed),
      ),
    )
    .toBe(true);
  await stop(agents.get(survivor.id));
  const rotated = await api(
    `/sites/${site.id}/agents/${survivor.id}/rotate`,
    "POST",
  );
  run({ id: survivor.id, token: rotated.token });
  await expect
    .poll(
      async () =>
        (await api("/agents")).filter((a: { online: boolean }) => a.online)
          .length,
    )
    .toBe(1);
  expect(
    await application.evaluate(async () =>
      (
        await fetch("/upload", { method: "POST", body: "after failover" })
      ).text(),
    ),
  ).toBe("14");
  await application.close();
  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
});
