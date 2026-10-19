import { test, expect } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import http from "node:http";
import { createHash } from "node:crypto";
const root = resolve(process.cwd(), ".."),
  url = "http://localhost:24880",
  password = "resource-e2e-password-123";
let dir: string,
  server: ChildProcess,
  agent: ChildProcess,
  secondAgent: ChildProcess,
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
  dir = mkdtempSync(resolve(tmpdir(), "tunnel-resource-e2e-"));
  writeFileSync(
    resolve(dir, "server.json"),
    JSON.stringify({
      listen: "127.0.0.1:24880",
      publicURL: url,
      accessURL: "http://apps.localhost:24880",
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
  const target = "http://127.0.0.1:24882";
  upstream = http.createServer((req, res) => {
    if (req.url === "/team/events") {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write("data: ready\n\n");
      const timer = setInterval(() => res.write(": heartbeat\n\n"), 500);
      req.on("close", () => clearInterval(timer));
      return;
    }
    if (req.url === "/team/login" && req.method === "POST") {
      req.resume();
      req.on("end", () => {
        if (
          req.headers.origin !== target ||
          !req.headers.referer?.startsWith(target + "/team/")
        ) {
          res.writeHead(403);
          res.end("CSRF origin mismatch");
          return;
        }
        res.writeHead(302, {
          "Set-Cookie":
            "inner_session=verified; Domain=internal.test; Path=/team/; HttpOnly",
          Location: target + "/team/",
        });
        res.end();
      });
      return;
    }
    if (req.url === "/team/client.js") {
      res.setHeader("Content-Type", "application/javascript");
      res.end("window.applicationReady=true");
      return;
    }
    if (req.url === "/team/whoami") {
      res.setHeader("Content-Type", "application/json");
      res.end(
        JSON.stringify({
          authenticated: req.headers.cookie?.includes("inner_session=verified"),
          tunnelSeen: req.headers.cookie?.includes("tunnel_") || false,
        }),
      );
      return;
    }
    if (req.url === "/team/upload" && req.method === "POST") {
      let size = 0;
      req.on("data", (b) => (size += b.length));
      req.on("end", () => res.end(String(size)));
      return;
    }
    if (req.url !== "/team/") {
      res.writeHead(404);
      res.end("private origin path");
      return;
    }
    res.setHeader("Content-Type", "text/html");
    res.end(
      req.headers.cookie?.includes("inner_session=verified")
        ? '<h1>Mounted internal console</h1><script src="/team/client.js"></script>'
        : '<h1>Internal application login</h1><form method="post" action="/team/login"><label>Application user<input name="user"></label><label>Application password<input name="password" type="password"></label><button>Sign in to application</button></form>',
    );
  });
  upstream.on("upgrade", (req, socket) => {
    if (req.url !== "/team/ws") {
      socket.destroy();
      return;
    }
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
  await new Promise<void>((done) => upstream.listen(24882, "127.0.0.1", done));
});
test.afterAll(async () => {
  await stop(agent);
  await stop(secondAgent);
  await stop(server);
  if (upstream) await new Promise<void>((done) => upstream.close(() => done()));
  if (dir) rmSync(dir, { recursive: true, force: true });
});

test("bulk resources, custom domain, mounted application login/upload and live move", async ({
  page,
}) => {
  await page.goto(url);
  await page.getByLabel("用户名", { exact: true }).fill("admin");
  await page.getByLabel("密码", { exact: true }).fill(password);
  await page.getByRole("button", { name: "登录控制台", exact: true }).click();
  await expect(page.locator(".profile")).toContainText("admin");
  async function site(name: string) {
    const r = await page.request.post(url + "/api/sites", {
      headers: { Origin: url },
      data: { name },
    });
    expect(r.status()).toBe(201);
    return r.json();
  }
  const a = await site("Source site"),
    b = await site("Destination site");
  for (const [i, s] of [a, b].entries()) {
    const path = resolve(dir, `agent-${i}.json`);
    writeFileSync(
      path,
      JSON.stringify({
        serverURL: url,
        siteID: s.site.id,
        allow: [{ cidr: "127.0.0.0/8", ports: [24882] }],
      }),
    );
    const child = start(["agent", "-c", path], { TUNNEL_AGENT_TOKEN: s.token });
    if (i === 0) agent = child;
    else secondAgent = child;
  }
  await expect
    .poll(async () => {
      const r = await page.request.get(url + "/api/sites");
      return (await r.json()).filter((s: { online: boolean }) => s.online)
        .length;
    })
    .toBe(2);
  await page.getByRole("button", { name: "批量应用", exact: true }).click();
  await page
    .getByLabel("批量应用名称 1", { exact: true })
    .fill("Mounted console");
  await page
    .getByLabel("批量内网地址 1", { exact: true })
    .fill("http://127.0.0.1:24882/team");
  await page
    .getByLabel("批量所属站点 1", { exact: true })
    .selectOption(a.site.id);
  await page.getByRole("button", { name: "增加一行", exact: true }).click();
  await page.getByLabel("批量应用名称 2", { exact: true }).fill("Auxiliary");
  await page
    .getByLabel("批量内网地址 2", { exact: true })
    .fill("http://127.0.0.1:24882/team/");
  await page
    .getByLabel("批量所属站点 2", { exact: true })
    .selectOption(a.site.id);
  await page.getByRole("button", { name: "批量创建", exact: true }).click();
  await expect(
    page.getByRole("cell", { name: "Mounted console", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: /^应用/ }).click();
  const row = page
    .getByRole("row")
    .filter({ has: page.getByText("Mounted console", { exact: true }) });
  await row.getByRole("button", { name: "编辑", exact: true }).click();
  const editor = page.getByRole("dialog");
  await editor
    .getByLabel("自定义访问域名", { exact: true })
    .fill("reports.customer.localhost");
  await editor.getByRole("button", { name: "保存", exact: true }).click();
  await expect(editor).toHaveCount(0);
  const popup = page.waitForEvent("popup");
  await row.getByRole("button", { name: "打开", exact: true }).click();
  const app = await popup;
  await expect(app).toHaveURL("http://reports.customer.localhost:24880/team/");
  await expect(app.getByRole("heading")).toHaveText(
    "Internal application login",
  );
  await app.getByLabel("Application user").fill("operator");
  await app.getByLabel("Application password").fill("internal-app-password");
  await app.getByRole("button", { name: "Sign in to application" }).click();
  await expect(app.getByRole("heading")).toHaveText("Mounted internal console");
  await expect
    .poll(() =>
      app.evaluate(
        () =>
          (window as unknown as { applicationReady: boolean }).applicationReady,
      ),
    )
    .toBe(true);
  expect(
    await app.evaluate(() => fetch("/team/whoami").then((r) => r.json())),
  ).toEqual({ authenticated: true, tunnelSeen: false });
  expect(
    await app.evaluate(() =>
      fetch("/team/upload", {
        method: "POST",
        body: new Uint8Array(1024 * 1024),
      }).then((r) => r.text()),
    ),
  ).toBe("1048576");
  expect(
    await app.evaluate(() => fetch("/private").then((r) => r.status)),
  ).toBe(404);

  await app.evaluate(() => {
    const state = { ws: false, sse: false, wsClosed: false, sseClosed: false };
    (window as unknown as { streams: typeof state }).streams = state;
    const ws = new WebSocket(
      location.origin.replace("http", "ws") + "/team/ws",
    );
    ws.onmessage = () => (state.ws = true);
    ws.onclose = () => (state.wsClosed = true);
    const sse = new EventSource("/team/events");
    sse.onmessage = () => (state.sse = true);
    sse.onerror = () => {
      state.sseClosed = true;
      sse.close();
    };
  });
  await expect
    .poll(() =>
      app.evaluate(() => {
        const s = (
          window as unknown as { streams: { ws: boolean; sse: boolean } }
        ).streams;
        return s.ws && s.sse;
      }),
    )
    .toBe(true);
  await page.getByRole("button", { name: "批量应用", exact: true }).click();
  await page.getByLabel("选择应用 Mounted console", { exact: true }).check();
  await page.getByLabel("选择应用 Auxiliary", { exact: true }).check();
  await page.getByLabel("迁移到站点", { exact: true }).selectOption(b.site.id);
  page.once("dialog", (d) => d.accept());
  await page.getByRole("button", { name: "迁移所选", exact: true }).click();
  await expect(
    page
      .getByRole("row")
      .filter({ has: page.getByText("Mounted console", { exact: true }) }),
  ).toContainText("Destination site");
  await expect
    .poll(() =>
      app.evaluate(() => {
        const s = (
          window as unknown as {
            streams: { wsClosed: boolean; sseClosed: boolean };
          }
        ).streams;
        return s.wsClosed && s.sseClosed;
      }),
    )
    .toBe(true);
  await app.reload();
  await expect(app.locator("body")).not.toContainText(
    "Mounted internal console",
  );
  await page.getByRole("button", { name: /^应用/ }).click();
  const reopened = page.waitForEvent("popup");
  await page
    .getByRole("row")
    .filter({ has: page.getByText("Mounted console", { exact: true }) })
    .getByRole("button", { name: "打开", exact: true })
    .click();
  const next = await reopened;
  await expect(next.getByRole("heading")).toHaveText(
    "Mounted internal console",
  );
  // Change the indexed route while the server remains running. The old
  // hostname must stop routing, and a fresh launch must use the new one.
  await page
    .getByRole("row")
    .filter({ has: page.getByText("Mounted console", { exact: true }) })
    .getByRole("button", { name: "编辑", exact: true })
    .click();
  await editor
    .getByLabel("自定义访问域名", { exact: true })
    .fill("updated.customer.localhost");
  await editor.getByRole("button", { name: "保存", exact: true }).click();
  await expect(editor).toHaveCount(0);
  expect(
    (
      await next.goto("http://reports.customer.localhost:24880/team/")
    )?.status(),
  ).toBe(404);
  const renamedPopup = page.waitForEvent("popup");
  await page
    .getByRole("row")
    .filter({ has: page.getByText("Mounted console", { exact: true }) })
    .getByRole("button", { name: "打开", exact: true })
    .click();
  const renamed = await renamedPopup;
  await expect(renamed).toHaveURL(
    "http://updated.customer.localhost:24880/team/",
  );
  await expect(renamed.getByRole("heading")).toHaveText(
    "Internal application login",
  );
  await renamed.getByLabel("Application user").fill("operator");
  await renamed
    .getByLabel("Application password")
    .fill("internal-app-password");
  await renamed.getByRole("button", { name: "Sign in to application" }).click();
  await expect(renamed.getByRole("heading")).toHaveText(
    "Mounted internal console",
  );
  await renamed.close();
  await next.close();
  await page.getByRole("button", { name: "批量应用", exact: true }).click();
  await page.getByLabel("选择应用 Mounted console", { exact: true }).check();
  await page.getByLabel("选择应用 Auxiliary", { exact: true }).check();
  page.once("dialog", (d) => d.accept());
  await page.getByRole("button", { name: "删除所选", exact: true }).click();
  await expect(
    page.getByRole("cell", { name: "Mounted console", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("cell", { name: "Auxiliary", exact: true }),
  ).toHaveCount(0);
  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
});
