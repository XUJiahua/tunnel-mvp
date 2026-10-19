import { test, expect } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import http from "node:http";
import { createHash } from "node:crypto";
const root = resolve(process.cwd(), ".."),
  url = "http://localhost:33880",
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
  dir = mkdtempSync(resolve(tmpdir(), "tunnel-delegated-roles-e2e-"));
  writeFileSync(
    resolve(dir, "server.json"),
    JSON.stringify({
      listen: "127.0.0.1:33880",
      publicURL: url,
      accessURL: "http://apps.localhost:33880",
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
  await new Promise<void>((done) => upstream.listen(33882, "127.0.0.1", done));
});
test.afterAll(async () => {
  for (const c of processes.reverse()) await stop(c);
  if (upstream) await new Promise<void>((done) => upstream.close(() => done()));
  if (dir) rmSync(dir, { recursive: true, force: true });
});
test("delegated organization roles UI, bounded permissions and live member revocation", async ({
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
  const org = await api("/organizations", "POST", {
      name: "Delegated organization",
    }),
    role = await api("/roles", "POST", {
      name: "Bounded delegator",
      organizationID: org.id,
      permissions: ["roles:manage", "applications:access", "audit:read"],
    });
  const manager = await api("/users", "POST", {
      username: "delegated-manager",
      organizationID: org.id,
      password,
    }),
    viewer = await api("/users", "POST", {
      username: "delegated-viewer",
      organizationID: org.id,
      password,
    });
  await api(`/users/${manager.id}/role`, "PATCH", { roleID: role.id });
  const { site, token } = await api("/sites", "POST", {
      name: "Delegated site",
      organizationID: org.id,
    }),
    app = await api("/services", "POST", {
      name: "Delegated app",
      siteID: site.id,
      url: "http://127.0.0.1:33882",
    });
  const config = resolve(dir, "agent.json");
  writeFileSync(
    config,
    JSON.stringify({
      serverURL: url,
      siteID: site.id,
      allow: [{ cidr: "127.0.0.0/8", ports: [33882] }],
    }),
  );
  start(["agent", "-c", config], { TUNNEL_AGENT_TOKEN: token });
  await expect
    .poll(
      async () =>
        (await api("/sites")).find((s: { id: string }) => s.id === site.id)
          .online,
    )
    .toBe(true);
  const managerContext = await browser.newContext(),
    operator = await managerContext.newPage();
  await login(operator, "delegated-manager");
  await operator
    .getByRole("button", { name: "组织与角色", exact: true })
    .click();
  await expect(
    operator.getByRole("button", { name: "创建组织", exact: true }),
  ).toHaveCount(0);
  await expect(
    operator.getByLabel("站点、令牌与 Agent 管理", { exact: true }),
  ).toHaveCount(0);
  await operator
    .getByLabel("角色名称", { exact: true })
    .fill("Delegated reader");
  await operator.getByLabel("访问组织内全部应用", { exact: true }).check();
  await operator.getByRole("button", { name: "创建角色", exact: true }).click();
  await expect(
    operator.locator("td strong").filter({ hasText: "Delegated reader" }),
  ).toBeVisible();
  const child = (
    await (await operator.request.get(url + "/api/roles")).json()
  ).roles.find((r: { name: string }) => r.name === "Delegated reader");
  operator.on("dialog", (dialog) => dialog.accept());
  await operator
    .getByLabel("角色 · delegated-viewer", { exact: true })
    .selectOption(child.id);
  const viewerContext = await browser.newContext(),
    member = await viewerContext.newPage();
  await login(member, "delegated-viewer");
  const launch = await (
      await member.request.post(url + `/api/services/${app.id}/launch`, {
        headers: { Origin: url },
      })
    ).json(),
    application = await viewerContext.newPage();
  await application.goto(launch.url);
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
  await operator
    .getByRole("row")
    .filter({
      has: operator.locator("strong").filter({ hasText: "Delegated reader" }),
    })
    .getByRole("button", { name: "编辑角色", exact: true })
    .click();
  await operator.getByLabel("访问组织内全部应用", { exact: true }).uncheck();
  await operator.getByRole("button", { name: "保存角色", exact: true }).click();
  await expect
    .poll(() => application.evaluate(() => (window as any).streamClosed))
    .toBe(true);
  expect((await member.request.get(url + "/api/me")).status()).toBe(401);
  const denied = await operator.request.post(url + "/api/roles", {
    headers: { Origin: url },
    data: {
      name: "Escalated",
      organizationID: org.id,
      permissions: ["sites:manage"],
    },
  });
  expect(denied.status()).toBe(403);
  await viewerContext.close();
  await managerContext.close();
});
