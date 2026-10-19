import { test, expect } from "@playwright/test";
import { spawn, execFileSync, type ChildProcess } from "node:child_process";
import { mkdtempSync, writeFileSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import http from "node:http";
import { createHash, generateKeyPairSync, sign } from "node:crypto";
const root = resolve(process.cwd(), ".."),
  url = "http://localhost:29880",
  password = "signed-upgrade-e2e-password-123";
let dir: string,
  server: ChildProcess,
  supervisor: ChildProcess,
  upstream: http.Server,
  releases: http.Server,
  envelope: Buffer;
const children: ChildProcess[] = [];
let candidate: Buffer;
let candidateV3: Buffer;
const invalid = Buffer.from("signed executable that cannot start\n");
const { publicKey, privateKey } = generateKeyPairSync("ed25519");
function manifest(
  sequence: number,
  version: string,
  binary: Buffer,
  path: string,
) {
  const payload = Buffer.from(
    JSON.stringify({
      format: 1,
      sequence,
      version,
      expiresAt: new Date(Date.now() + 3600000).toISOString(),
      artifacts: [
        {
          platform: `${process.platform}/${process.arch === "x64" ? "amd64" : process.arch}`,
          url: `http://localhost:29883/${path}`,
          sha256: createHash("sha256").update(binary).digest("hex"),
          size: binary.length,
        },
      ],
    }),
  );
  return Buffer.from(
    JSON.stringify({
      payload: payload.toString("base64"),
      signature: sign(null, payload, privateKey).toString("base64"),
    }),
  );
}
function start(args: string[], env: Record<string, string> = {}) {
  const c = spawn(resolve(root, "bin/tunnel"), args, {
    env: { ...process.env, ...env },
    stdio: "ignore",
  });
  children.push(c);
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
  test.setTimeout(120000);
  dir = mkdtempSync(resolve(tmpdir(), "tunnel-upgrade-e2e-"));
  execFileSync(
    process.env.GO_BINARY || "go",
    [
      "build",
      "-trimpath",
      "-ldflags",
      "-X main.version=agent-upgrade-v2",
      "-o",
      resolve(dir, "candidate"),
      "./cmd/tunnel",
    ],
    { cwd: root, stdio: "pipe" },
  );
  candidate = readFileSync(resolve(dir, "candidate"));
  execFileSync(
    process.env.GO_BINARY || "go",
    [
      "build",
      "-trimpath",
      "-ldflags",
      "-X main.version=agent-upgrade-v3",
      "-o",
      resolve(dir, "candidate-v3"),
      "./cmd/tunnel",
    ],
    { cwd: root, stdio: "pipe" },
  );
  candidateV3 = readFileSync(resolve(dir, "candidate-v3"));
  writeFileSync(
    resolve(dir, "release.pub"),
    publicKey
      .export({ type: "spki", format: "der" })
      .subarray(-32)
      .toString("base64"),
    { mode: 0o600 },
  );
  envelope = manifest(1, "agent-upgrade-v2", candidate, "candidate");
  releases = http.createServer((req, res) => {
    res.end(
      req.url === "/manifest"
        ? envelope
        : req.url === "/candidate"
          ? candidate
          : req.url === "/candidate-v3"
            ? candidateV3
            : invalid,
    );
  });
  await new Promise<void>((done) => releases.listen(29883, "127.0.0.1", done));
  writeFileSync(
    resolve(dir, "server.json"),
    JSON.stringify({
      listen: "127.0.0.1:29880",
      publicURL: url,
      accessURL: "http://apps.localhost:29880",
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
    let n = 0;
    req.on("data", (b) => (n += b.length));
    req.on("end", () =>
      res.end(
        req.method === "POST" ? String(n) : "<h1>Upgrade application</h1>",
      ),
    );
  });
  await new Promise<void>((done) => upstream.listen(29882, "127.0.0.1", done));
});
test.afterAll(async () => {
  for (const c of children.reverse()) await stop(c);
  if (upstream) await new Promise<void>((done) => upstream.close(() => done()));
  if (releases) await new Promise<void>((done) => releases.close(() => done()));
  if (dir) rmSync(dir, { recursive: true, force: true });
});
test("signed real Agent upgrade, failed executable automatic recovery, replay guard and offline rollback", async ({
  page,
  context,
}) => {
  test.setTimeout(90000);
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
  const { site } = await api("/sites", "POST", { name: "Managed update site" }),
    registration = await api(`/sites/${site.id}/agents`, "POST", {
      name: "Managed Agent",
    }),
    app = await api("/services", "POST", {
      name: "Upgrade app",
      siteID: site.id,
      url: "http://127.0.0.1:29882",
    });
  const config = resolve(dir, "agent.json"),
    updateDir = resolve(dir, "updates");
  writeFileSync(
    config,
    JSON.stringify({
      serverURL: url,
      siteID: site.id,
      instanceID: registration.agent.id,
      allow: [{ cidr: "127.0.0.0/8", ports: [29882] }],
    }),
  );
  const args = [
    "agent",
    "supervise",
    "--config",
    config,
    "--update-dir",
    updateDir,
    "--bootstrap-binary",
    resolve(root, "bin/tunnel"),
    "--release-public-key",
    resolve(dir, "release.pub"),
    "--manifest-url",
    "http://localhost:29883/manifest",
    "--update-interval",
    "10s",
    "--ready-timeout",
    "5s",
  ];
  supervisor = start(args, { TUNNEL_AGENT_TOKEN: registration.token });
  await expect
    .poll(async () => (await api("/agents"))[0].reportedVersion, {
      timeout: 20000,
    })
    .toBe("agent-upgrade-v2");
  await expect
    .poll(
      () =>
        JSON.parse(readFileSync(resolve(updateDir, "journal.json"), "utf8"))
          .pending,
    )
    .toBe(false);
  const launch = await api(`/services/${app.id}/launch`, "POST"),
    application = await context.newPage();
  await application.goto(launch.url);
  await expect(
    application.getByRole("heading", { name: "Upgrade application" }),
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
    const w = window as any;
    void (async () => {
      try {
        const r = await fetch("/events"),
          reader = r.body!.getReader();
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
  envelope = manifest(2, "broken-release", invalid, "invalid");
  await expect
    .poll(
      () =>
        JSON.parse(readFileSync(resolve(updateDir, "journal.json"), "utf8"))
          .highWater,
      { timeout: 20000 },
    )
    .toBe(2);
  await expect
    .poll(
      () =>
        JSON.parse(readFileSync(resolve(updateDir, "journal.json"), "utf8"))
          .pending,
      { timeout: 15000 },
    )
    .toBe(false);
  await expect
    .poll(
      async () => {
        const a = (await api("/agents"))[0];
        return a.online && a.reportedVersion === "agent-upgrade-v2";
      },
      { timeout: 10000 },
    )
    .toBe(true);
  await expect
    .poll(() => application.evaluate(() => (window as any).streamClosed))
    .toBe(true);
  expect(
    await application.evaluate(async () =>
      (
        await fetch("/upload", { method: "POST", body: "after rollback" })
      ).text(),
    ),
  ).toBe("14");
  // A failed sequence is never automatically retried, including after restart.
  await stop(supervisor);
  supervisor = start(args, { TUNNEL_AGENT_TOKEN: registration.token });
  await expect
    .poll(
      async () => {
        const a = (await api("/agents"))[0];
        return a.online && a.reportedVersion === "agent-upgrade-v2";
      },
      { timeout: 15000 },
    )
    .toBe(true);
  expect(
    JSON.parse(readFileSync(resolve(updateDir, "journal.json"), "utf8"))
      .highWater,
  ).toBe(2);
  // Healthy v3 update creates an explicit rollback slot; exercise offline CLI rollback.
  envelope = manifest(3, "agent-upgrade-v3", candidateV3, "candidate-v3");
  await expect
    .poll(
      () =>
        JSON.parse(readFileSync(resolve(updateDir, "journal.json"), "utf8"))
          .highWater,
      { timeout: 20000 },
    )
    .toBe(3);
  await expect
    .poll(
      () =>
        JSON.parse(readFileSync(resolve(updateDir, "journal.json"), "utf8"))
          .pending,
      { timeout: 10000 },
    )
    .toBe(false);
  await expect
    .poll(async () => (await api("/agents"))[0].reportedVersion, {
      timeout: 10000,
    })
    .toBe("agent-upgrade-v3");
  await stop(supervisor);
  execFileSync(
    resolve(root, "bin/tunnel"),
    ["agent", "rollback", "--update-dir", updateDir],
    { stdio: "pipe" },
  );
  expect(
    JSON.parse(readFileSync(resolve(updateDir, "journal.json"), "utf8"))
      .highWater,
  ).toBe(3);
  supervisor = start(args, { TUNNEL_AGENT_TOKEN: registration.token });
  await expect
    .poll(
      async () => {
        const a = (await api("/agents"))[0];
        return a.online && a.reportedVersion === "agent-upgrade-v2";
      },
      { timeout: 10000 },
    )
    .toBe(true);
  await application.close();
});
