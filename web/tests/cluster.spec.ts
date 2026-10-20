import { test, expect, type Page } from "@playwright/test";
import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import http from "node:http";
import type { Socket } from "node:net";

const enabled = process.env.TUNNEL_TEST_STORAGE === "postgres" && !!process.env.POSTGRES_CONTAINER;
const root = resolve(process.cwd(), ".."), current = resolve(root, "bin/tunnel");
const legacy = process.env.TUNNEL_TEST_HA3_BINARY || "";
const publicURL = "http://localhost:27880", password = "cluster-browser-test-password-123";
const nodeIDs = ["browser-a", "browser-b"];
const ports = [27881, 27882], privatePorts = [27883, 27884];
let dir = "", activeDSN = process.env.TUNNEL_POSTGRES_DSN || "";
let selected = 1, applicationNode = 0, commits = 0;
let lb: http.Server | undefined, upstream: http.Server | undefined;
const sockets = new Set<Socket>(), children: ChildProcess[] = [];
const nodes: Array<ChildProcess | undefined> = [], agents: Array<ChildProcess | undefined> = [];
const targets = new Map<string, number>();
let dataDirs: string[] = [], agentConfigs: string[] = [], agentTokens: string[] = [];
type NodeStatus = { nodeID: string; fresh: boolean; draining: boolean; report?: { backlog?: { pending: number } } };

function command(binary: string, args: string[], env: Record<string, string> = {}) {
  const result = spawnSync(binary, args, { env: { ...process.env, TUNNEL_POSTGRES_DSN: activeDSN, ...env }, encoding: "utf8", timeout: 20000 });
  expect(result.status, `${binary} ${args.join(" ")}: ${result.stdout} ${result.stderr}`).toBe(0);
}
function start(binary: string, args: string[], name: string, env: Record<string, string> = {}) {
  const child = spawn(binary, args, { env: { ...process.env, TUNNEL_POSTGRES_DSN: activeDSN, ...env }, stdio: "pipe" });
  let log = "";
  const capture = (chunk: Buffer) => {
    log = (log + chunk.toString()).slice(-200000);
    writeFileSync(resolve(dir, name + ".log"), log);
  };
  child.stdout?.on("data", capture); child.stderr?.on("data", capture);
  children.push(child);
  return child;
}
async function stop(child?: ChildProcess) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  await new Promise<void>((done, reject) => {
    const timer = setTimeout(() => { child.kill("SIGKILL"); reject(new Error("process shutdown exceeded 12 seconds")); }, 12000);
    child.once("exit", () => { clearTimeout(timer); done(); }); child.kill("SIGTERM");
  });
}
function target(req: http.IncomingMessage) {
  if (req.url?.startsWith("/tunnel/")) return targets.get(String(req.headers["x-tunnel-instance"] || "")) ?? 1;
  return req.headers.host === "localhost:27880" ? selected : applicationNode;
}
function forward(req: http.IncomingMessage) {
  return http.request({ hostname: "127.0.0.1", port: ports[target(req)], path: req.url, method: req.method, headers: req.headers });
}
async function startNode(index: number, binary: string) {
  const path = resolve(dir, `server-${index}.json`);
  writeFileSync(path, JSON.stringify({ listen: `127.0.0.1:${ports[index]}`, publicURL, accessURL: "http://apps.localhost:27880", storage: "postgres", dataDir: dataDirs[index], cluster: {
    enabled: true, nodeID: nodeIDs[index], listen: `127.0.0.1:${privatePorts[index]}`, advertiseURL: `https://127.0.0.1:${privatePorts[index]}`,
    caFile: resolve(dir, "pki/ca.crt"), certFile: resolve(dir, "pki", nodeIDs[index], "node.crt"), keyFile: resolve(dir, "pki", nodeIDs[index], "node.key"),
  } }));
  nodes[index] = start(binary, ["server", "-c", path], `server-${index}`, { TUNNEL_ADMIN_PASSWORD: password });
  await expect.poll(async () => {
    if (nodes[index]?.exitCode !== null) throw new Error(readFileSync(resolve(dir, `server-${index}.log`), "utf8"));
    try {
      return await new Promise<boolean>((done) => {
        const req = http.get({ host: "127.0.0.1", port: ports[index], path: "/healthz", headers: { Host: "localhost:27880" }, timeout: 1000 }, (res) => { res.resume(); done(res.statusCode === 200); });
        req.on("error", () => done(false)); req.on("timeout", () => { req.destroy(); done(false); });
      });
    } catch { return false; }
  }, { timeout: 20000 }).toBe(true);
}
async function api(page: Page, path: string, body?: unknown) {
  const response = body === undefined ? await page.request.get(publicURL + "/api" + path) : await page.request.post(publicURL + "/api" + path, { headers: { Origin: publicURL }, data: body });
  expect(response.ok(), `${path}: ${await response.text()}`).toBe(true);
  return response.json();
}
async function login(page: Page) {
  await page.goto(publicURL);
  await page.getByLabel("用户名", { exact: true }).fill("admin");
  await page.getByLabel("密码", { exact: true }).fill(password);
  await page.getByRole("button", { name: "登录控制台", exact: true }).click();
  await expect(page.locator(".profile")).toContainText("admin");
}
async function online(page: Page, id: string) {
  await expect.poll(async () => (await api(page, "/agents")).some((a: { id: string; online: boolean }) => a.id === id && a.online), { timeout: 20000 }).toBe(true);
}
async function converged(page: Page) {
  await expect.poll(async () => {
    const result = await api(page, "/cluster/status");
    return result.nodes?.length === 2 && result.nodes.every((n: NodeStatus) => n.fresh && n.report?.backlog?.pending === 0);
  }, { timeout: 20000 }).toBe(true);
}

// These are real CLI processes, private mTLS, Agents, a load balancer and an
// actual browser. No intercepted API responses or simulated old-version reader.
test.describe("HA-4 real mixed-version cluster", () => {
  test.skip(!enabled, "requires isolated PostgreSQL CI container");
  test.beforeAll(async () => {
    expect(legacy, "pinned HA-3 CLI must be supplied").not.toBe("");
    dir = mkdtempSync(resolve(tmpdir(), "tunnel-cluster-browser-"));
    dataDirs = nodeIDs.map((id) => resolve(dir, id));
    command("bash", [resolve(root, "scripts/create-cluster-pki.sh"), resolve(dir, "pki"), ...nodeIDs.map((id) => id + "=127.0.0.1")]);
    lb = http.createServer((req, res) => {
      const proxy = forward(req);
      proxy.on("response", (remote) => { res.writeHead(remote.statusCode || 502, remote.headers); remote.pipe(res); });
      proxy.on("error", () => { if (!res.headersSent) res.writeHead(502); res.end("node unavailable"); }); req.pipe(proxy);
      res.on("close", () => proxy.destroy());
    });
    lb.on("connection", (socket) => { sockets.add(socket); socket.on("close", () => sockets.delete(socket)); });
    lb.on("upgrade", (req, socket, head) => {
      const proxy = forward(req); proxy.on("error", () => socket.destroy());
      proxy.on("upgrade", (response, remote, remoteHead) => {
        socket.write(`HTTP/1.1 ${response.statusCode} ${response.statusMessage}\r\n` + response.rawHeaders.reduce((all, value, i) => all + (i % 2 ? value + "\r\n" : value + ": "), "") + "\r\n");
        if (head.length) remote.write(head); if (remoteHead.length) socket.write(remoteHead);
        socket.pipe(remote); remote.pipe(socket); socket.on("close", () => remote.destroy()); remote.on("close", () => socket.destroy());
      });
      proxy.on("response", (response) => { response.resume(); socket.destroy(); }); proxy.end();
    });
    await new Promise<void>((done) => lb!.listen(27880, done));
    upstream = http.createServer((req, res) => {
      if (req.url === "/events") {
        res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
        let i = 0; res.write(`data: ${i++}\n\n`); const timer = setInterval(() => res.write(`data: ${i++}\n\n`), 200);
        res.on("close", () => clearInterval(timer)); return;
      }
      if (req.url === "/commit" && req.method === "POST") { commits++; res.end("committed"); return; }
      res.writeHead(200, { "Content-Type": "text/html" });
      res.end('<h1>cluster-live</h1><div id="events">waiting</div><script>const source=new EventSource("/events");source.onmessage=e=>document.getElementById("events").textContent=e.data;</script>');
    });
    await new Promise<void>((done) => upstream!.listen(27885, "127.0.0.1", done));
    await startNode(0, legacy); await startNode(1, current);
  });
  test.afterAll(async ({}, info) => {
    for (const child of [...agents, ...nodes]) await stop(child);
    for (const socket of sockets) socket.destroy();
    if (lb) await new Promise<void>((done) => lb!.close(() => done()));
    if (upstream) await new Promise<void>((done) => upstream!.close(() => done()));
    if (dir && info.status !== info.expectedStatus) {
      for (let i = 0; i < 2; i++) { try { await info.attach(`node-${i}`, { body: readFileSync(resolve(dir, `server-${i}.log`)), contentType: "text/plain" }); } catch {} }
    }
    if (dir) rmSync(dir, { recursive: true, force: true });
  });

  test("mixed routing, drain-preserved SSE, rolling rollback and paired fresh-database recovery", async ({ page, context }) => {
    test.setTimeout(180000);
    await login(page);
    const site = await api(page, "/sites", { name: "Cluster recovery site" });
    const app = await api(page, "/services", { name: "Cluster recovery application", siteID: site.site.id, url: "http://127.0.0.1:27885" });
    const registrations = [];
    for (const index of [1, 0]) {
      const registration = await api(page, `/sites/${site.site.id}/agents`, { name: `owner-${index}` });
      registrations[index] = registration; targets.set(registration.agent.id, index);
      const config = resolve(dir, `agent-${index}.json`); agentConfigs[index] = config; agentTokens[index] = registration.token;
      writeFileSync(config, JSON.stringify({ serverURL: publicURL, siteID: site.site.id, instanceID: registration.agent.id, allow: [{ cidr: "127.0.0.0/8", ports: [27885] }] }));
    }
    agents[1] = start(current, ["agent", "-c", agentConfigs[1]], "agent-1", { TUNNEL_AGENT_TOKEN: agentTokens[1] });
    await online(page, registrations[1].agent.id);
    await page.getByRole("button", { name: "监控历史", exact: true }).click();
    const clusterPanel = page.getByRole("region", { name: "集群节点" });
    await expect(clusterPanel).toContainText("观测覆盖不完整");
    await expect(clusterPanel.getByRole("row").filter({ hasText: "browser-a" })).toContainText("未上报");
    const firstLaunch = await api(page, `/services/${app.id}/launch`, {});
    const application = await context.newPage(); await application.goto(firstLaunch.url);
    await expect(application.getByRole("heading", { name: "cluster-live" })).toBeVisible();
    await expect.poll(() => application.locator("#events").textContent()).not.toBe("waiting");
    await api(page, `/services/${app.id}/probe`, {});
    await expect.poll(async () => (await api(page, `/services/${app.id}/history`)).samples.some((s: { nodeID: string }) => s.nodeID === "browser-b")).toBe(true);
    await expect.poll(async () => (await api(page, `/audit?nodeID=browser-b&event=probe&resourceID=${app.id}`)).entries.length).toBeGreaterThan(0);
    const marker = await api(page, `/audit?nodeID=browser-b&event=probe&resourceID=${app.id}`);
    expect(marker.entries.length).toBeGreaterThan(0);
    const eventID = marker.entries[0].eventID;
    const beforeSamples = (await api(page, `/services/${app.id}/history`)).samples;
    const sampleID = beforeSamples.find((s: { nodeID: string }) => s.nodeID === "browser-b").eventID;
    expect(eventID).toMatch(/^[a-f0-9]{64}$/); expect(sampleID).toMatch(/^[a-f0-9]{64}$/);

    await api(page, "/cluster/local/drain", { nodeID: "browser-b", draining: true });
    expect((await page.request.get(publicURL + "/healthz")).status()).toBe(503);
    const count = Number(await application.locator("#events").textContent());
    await expect.poll(async () => Number(await application.locator("#events").textContent())).toBeGreaterThan(count + 5);
    agents[0] = start(current, ["agent", "-c", agentConfigs[0]], "agent-0", { TUNNEL_AGENT_TOKEN: agentTokens[0] });
    await online(page, registrations[0].agent.id);
    const secondLaunch = await api(page, `/services/${app.id}/launch`, {});
    const second = await context.newPage(); await second.goto(secondLaunch.url);
    await expect(second.getByRole("heading", { name: "cluster-live" })).toBeVisible();
    expect(await second.evaluate(() => fetch("/commit", { method: "POST", body: "one execution" }).then((r) => r.text()))).toBe("committed");
    expect(commits).toBe(1);

    // A upgrades while B's management listener and existing owner channels live.
    await stop(nodes[0]); await startNode(0, current); selected = 0;
    await converged(page);
    await api(page, "/cluster/local/drain", { nodeID: "browser-a", draining: false });
    selected = 1; await api(page, "/cluster/local/drain", { nodeID: "browser-b", draining: false }); selected = 0;
    await page.reload(); await page.getByRole("button", { name: "监控历史", exact: true }).click();
    await expect(clusterPanel).toContainText("全部节点正在上报", { timeout: 20000 });
    await expect(page.getByRole("columnheader", { name: "来源节点", exact: true })).toBeVisible();
    await page.getByRole("button", { name: "审计日志", exact: true }).click();
    await expect(page.getByLabel("来源节点", { exact: true })).toBeVisible();
    await page.getByLabel("来源节点", { exact: true }).fill("browser-b");
    await page.getByRole("button", { name: "查询审计", exact: true }).click();
    await expect(page.getByRole("cell", { name: "browser-b", exact: true }).first()).toBeVisible();

    // Roll B back to the actual HA-3 executable and then upgrade it again.
    selected = 1; await api(page, "/cluster/local/drain", { nodeID: "browser-b", draining: true }); selected = 0;
    await stop(nodes[1]); await startNode(1, legacy);
    await expect.poll(async () => (await api(page, "/cluster/status")).nodes.find((n: NodeStatus) => n.nodeID === "browser-b")?.fresh).toBe(false);
    await online(page, registrations[1].agent.id);
    await stop(nodes[1]); await startNode(1, current); await converged(page);
    await application.close(); await second.close();
    for (const index of [0, 1]) { selected = index; expect((await api(page, `/audit?event=probe&resourceID=${app.id}`)).entries.filter((e: { eventID: string }) => e.eventID === eventID)).toHaveLength(1); }

    // Full offline pair: configuration plus each node's operational archive.
    for (const child of agents) await stop(child); for (const child of nodes) await stop(child);
    const backup = resolve(dir, "state-backup.json");
    command(current, ["storage", "recover-postgres-runtime", "--storage", "postgres", "--data-dir", dataDirs[0], "--old-server-stopped"]);
    command(current, ["storage", "export", "--storage", "postgres", "--data-dir", dataDirs[0], "--output", backup]);
    for (const index of [0, 1]) command(current, ["storage", "export-operations", "--data-dir", dataDirs[index], "--output", resolve(dir, `operations-${index}.tar.gz`)]);
    const dsn = new URL(activeDSN), database = dsn.pathname.slice(1);
    expect(database).toMatch(/^tunnel_browser_\d+$/);
    const restoredDatabase = database + "_restored";
    command("docker", ["exec", process.env.POSTGRES_CONTAINER!, "createdb", "-U", "postgres", restoredDatabase]);
    dsn.pathname = "/" + restoredDatabase; activeDSN = dsn.toString();
    dataDirs = nodeIDs.map((id) => resolve(dir, "restored", id));
    mkdirSync(resolve(dir, "restored"));
    command(current, ["storage", "restore", "--storage", "postgres", "--data-dir", dataDirs[0], "--input", backup]);
    for (const index of [0, 1]) command(current, ["storage", "restore-operations", "--data-dir", dataDirs[index], "--input", resolve(dir, `operations-${index}.tar.gz`)]);
    await startNode(0, current); await startNode(1, current); selected = 0;
    expect((await page.request.get(publicURL + "/api/me")).status()).toBe(401);
    await login(page);
    for (const index of [0, 1]) agents[index] = start(current, ["agent", "-c", agentConfigs[index]], `agent-restored-${index}`, { TUNNEL_AGENT_TOKEN: agentTokens[index] });
    for (const registration of registrations) await online(page, registration.agent.id);
    await converged(page);
    const assertRestored = async () => {
      for (const index of [0, 1]) {
        selected = index;
        expect((await api(page, `/audit?event=probe&resourceID=${app.id}`)).entries.filter((e: { eventID: string }) => e.eventID === eventID)).toHaveLength(1);
        expect((await api(page, `/services/${app.id}/history`)).samples.filter((s: { eventID: string }) => s.eventID === sampleID)).toHaveLength(1);
      }
    };
    await assertRestored();
    selected = 0; await stop(nodes[1]); await startNode(1, current); await converged(page); await assertRestored();
    const restoredLaunch = await api(page, `/services/${app.id}/launch`, {});
    const restoredApp = await context.newPage(); await restoredApp.goto(restoredLaunch.url);
    await expect(restoredApp.getByRole("heading", { name: "cluster-live" })).toBeVisible();
    expect(await restoredApp.evaluate(() => fetch("/commit", { method: "POST", body: "second execution" }).then((r) => r.text()))).toBe("committed"); expect(commits).toBe(2);
    await restoredApp.close();
    await page.goto(publicURL); await page.getByRole("button", { name: "监控历史", exact: true }).click();
    await expect(page.getByRole("region", { name: "集群节点" })).toContainText("全部节点正在上报", { timeout: 20000 });
  });
});
