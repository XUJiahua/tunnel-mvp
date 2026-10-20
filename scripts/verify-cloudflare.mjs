#!/usr/bin/env node
// Disposable public HTTPS/WSS simulation. No production configuration is read.
import { spawn, execFileSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, readFileSync, existsSync, rmSync, openSync, closeSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';
import { createHash, randomBytes } from 'node:crypto';
import http from 'node:http';
import assert from 'node:assert/strict';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const require = createRequire(resolve(root, 'web/package.json'));
const { chromium } = require('@playwright/test');
const args = process.argv.slice(2);
const option = (key, fallback) => args.includes(key) ? args[args.indexOf(key) + 1] : fallback;
const output = option('--output', '');
const binary = resolve(option('--binary', resolve(root, 'bin/tunnel')));
const cloudflared = option('--cloudflared', 'cloudflared');
if (!output || existsSync(output)) throw Error('--output must name a new report file');
const directory = mkdtempSync(resolve(tmpdir(), 'tunnel-cloudflare-'));
const password = randomBytes(24).toString('base64url');
const children = [], logFDs = [], upstreams = [], sockets = new Set();
const report = {
  generatedAt: new Date().toISOString(), status: 'running', stages: [],
  sourceCommit: execFileSync('git', ['rev-parse', 'HEAD'], { cwd: root, encoding: 'utf8' }).trim(),
  topology: 'One host; separate Server and four Agent processes, two synthetic sites. Console and two applications use separate Cloudflare Quick Tunnels. Agent WSS and browser HTTPS/WSS traverse public Cloudflare edges.',
  limitations: ['Single host/process isolation, not two physical private networks', 'Cloudflare Quick Tunnels do not support SSE; SSE is not tested', 'Not an HA, production capacity, real business application or IdP acceptance'],
};
let stage = 'initialization', browser;
const delay = ms => new Promise(r => setTimeout(r, ms));
async function wait(predicate, seconds = 60) {
  const deadline = Date.now() + seconds * 1000;
  while (Date.now() < deadline) {
    if (await predicate()) return;
    await delay(250);
  }
  throw Error('readiness deadline exceeded');
}
function start(executable, parameters, name, extra = {}) {
  const env = Object.fromEntries(Object.entries(process.env).filter(([k]) => !/^(TUNNEL_|CLOUDFLARE_|TUNNEL_TOKEN$)/.test(k)));
  const log = resolve(directory, `${name}.log`), fd = openSync(log, 'w', 0o600);
  logFDs.push(fd);
  const child = spawn(executable, parameters, { env: { ...env, ...extra }, stdio: ['ignore', fd, fd] });
  child.on('error', () => {});
  children.push(child);
  return { child, log };
}
async function stop(child) {
  if (!child?.pid || child.exitCode !== null || child.signalCode !== null) return;
  const exited = new Promise(resolve => child.once('exit', resolve));
  child.kill('SIGTERM');
  await Promise.race([exited, delay(10000)]);
  if (child.exitCode === null && child.signalCode === null) {
    child.kill('SIGKILL');
    await Promise.race([exited, delay(3000)]);
  }
}
function done(name) { report.stages.push({ name, result: 'pass' }); console.log(`PASS ${name}`); }
async function freePort() {
  const reservation = http.createServer();
  await new Promise(r => reservation.listen(0, '127.0.0.1', r));
  const port = reservation.address().port;
  await new Promise(r => reservation.close(r));
  return port;
}
async function publicTunnel(name, port) {
  const process = start(cloudflared, ['tunnel', '--no-autoupdate', '--protocol', 'http2', '--url', `http://127.0.0.1:${port}`, '--metrics', '127.0.0.1:0'], name);
  let url;
  await wait(() => {
    const text = readFileSync(process.log, 'utf8');
    // A failure log can contain https://api.trycloudflare.com/tunnel. That
    // provider API is not an allocated public application hostname.
    url = text.match(/https:\/\/(?!api\.)[a-z0-9-]+\.trycloudflare\.com\b/)?.[0];
    if (process.child.exitCode !== null || process.child.signalCode !== null) {
      report.cloudflaredFailure = text.includes('network is unreachable') ? 'network_unreachable' : text.includes('lookup ') ? 'dns_failure' : 'provider_or_connector_failure';
      throw Error('cloudflared exited before readiness');
    }
    return !!url;
  });
  return url;
}
async function upstream(name) {
  const server = http.createServer((req, res) => {
    if (req.url === '/login' && req.method === 'POST') {
      req.resume(); req.on('end', () => {
        res.writeHead(303, { 'Set-Cookie': `sim_auth=${name}; Domain=127.0.0.1; Path=/; HttpOnly; SameSite=Lax`, Location: `http://127.0.0.1:${server.address().port}/private` }); res.end();
      }); return;
    }
    if (req.url === '/login') {
      res.setHeader('Content-Type', 'text/html');
      res.end(`<h1>Login ${name}</h1><form method="post" action="/login"><button>Sign in ${name}</button></form>`); return;
    }
    if (!String(req.headers.cookie).includes(`sim_auth=${name}`)) {
      res.writeHead(302, { Location: `http://127.0.0.1:${server.address().port}/login` }); res.end(); return;
    }
    if (req.url === '/upload') {
      let bytes = 0; req.on('data', b => bytes += b.length); req.on('end', () => res.end(String(bytes))); return;
    }
    res.setHeader('Content-Type', 'text/html'); res.end(`<h1>Private ${name}</h1>`);
  });
  server.on('connection', socket => { sockets.add(socket); socket.on('close', () => sockets.delete(socket)); });
  server.on('upgrade', (req, socket) => {
    if (!String(req.headers.cookie).includes(`sim_auth=${name}`)) { socket.end('HTTP/1.1 401 Unauthorized\r\n\r\n'); return; }
    const accept = createHash('sha1').update(String(req.headers['sec-websocket-key']) + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').digest('base64');
    socket.write('HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ' + accept + '\r\n\r\n');
    let pending = Buffer.alloc(0);
    socket.on('data', bytes => {
      pending = Buffer.concat([pending, bytes]);
      while (pending.length >= 2) {
        const opcode = pending[0] & 15, masked = !!(pending[1] & 128), length = pending[1] & 127;
        if (length >= 126) { socket.destroy(); return; }
        const offset = 2 + (masked ? 4 : 0);
        if (pending.length < offset + length) return;
        const payload = Buffer.from(pending.subarray(offset, offset + length));
        if (masked) for (let i = 0; i < length; i++) payload[i] ^= pending[2 + (i % 4)];
        pending = pending.subarray(offset + length);
        if (opcode === 8) { socket.end(Buffer.from([0x88, 0])); return; }
        if (opcode === 1) { const reply = Buffer.from(`echo:${payload.toString()}`); socket.write(Buffer.concat([Buffer.from([0x81, reply.length]), reply])); }
      }
    });
    socket.on('error', () => {}); socket.on('end', () => socket.end());
  });
  await new Promise(r => server.listen(0, '127.0.0.1', r));
  upstreams.push(server);
  return server.address().port;
}

try {
  stage = 'public tunnel allocation';
  const port = await freePort();
  // Allocate serially to keep failures and provider rate limits attributable.
  const consoleURL = await publicTunnel('console-edge', port);
  const appURLs = [await publicTunnel('application-a-edge', port), await publicTunnel('application-b-edge', port)];
  report.urls = { console: consoleURL, applications: appURLs };
  done(stage);
  stage = 'public HTTPS control plane and embedded UI';
  const config = resolve(directory, 'server.json');
  writeFileSync(config, JSON.stringify({ listen: `127.0.0.1:${port}`, publicURL: consoleURL, accessURL: 'https://apps.simulation.invalid', dataDir: resolve(directory, 'data'), storage: 'sqlite', probeIntervalSeconds: 0 }), { mode: 0o600 });
  start(binary, ['server', '-c', config], 'server', { TUNNEL_ADMIN_PASSWORD: password });
  await wait(async () => { try { return (await fetch(consoleURL + '/healthz', { signal: AbortSignal.timeout(5000) })).ok; } catch { return false; } }, 90);
  browser = await chromium.launch(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH, args: ['--no-sandbox'] } : {});
  const context = await browser.newContext();
  const page = await context.newPage();
  page.setDefaultTimeout(30000);
  await page.goto(consoleURL);
  await page.getByLabel('用户名', { exact: true }).fill('admin');
  await page.getByLabel('密码', { exact: true }).fill(password);
  await page.getByRole('button', { name: '登录控制台', exact: true }).click();
  await page.locator('.profile').filter({ hasText: 'admin' }).waitFor();
  async function api(path, method = 'GET', data, client = context.request) {
    const r = await client.fetch(consoleURL + '/api' + path, { method, headers: { Origin: consoleURL }, data, timeout: 30000 });
    assert(r.ok(), `${method} ${path}: HTTP ${r.status()}`);
    return r.json();
  }
  done(stage);
  stage = 'two synthetic sites and four public WSS Agent connections';
  const applications = [], agents = [], sites = [];
  for (let index = 0; index < 2; index++) {
    const name = index ? 'site-b' : 'site-a', originPort = await upstream(name);
    const { site } = await api('/sites', 'POST', { name }); sites.push(site);
    for (let instance = 0; instance < 2; instance++) {
      const registration = await api(`/sites/${site.id}/agents`, 'POST', { name: `${name}-${instance}` });
      const path = resolve(directory, `${registration.agent.id}.json`);
      writeFileSync(path, JSON.stringify({ serverURL: consoleURL, siteID: site.id, instanceID: registration.agent.id, allow: [{ cidr: '127.0.0.0/8', ports: [originPort] }] }), { mode: 0o600 });
      const process = start(binary, ['agent', '-c', path], registration.agent.id, { TUNNEL_AGENT_TOKEN: registration.token });
      agents.push({ process, path, token: registration.token });
    }
    applications.push(await api('/services', 'POST', { name, siteID: site.id, url: `http://127.0.0.1:${originPort}`, customDomain: new URL(appURLs[index]).hostname }));
  }
  await wait(async () => (await api('/agents')).filter(a => a.online).length === 4, 90);
  done(stage);
  stage = 'public application login redirects secure cookies and uploads';
  const appPages = [];
  for (let index = 0; index < 2; index++) {
    const launch = await api(`/services/${applications[index].id}/launch`, 'POST');
    assert.equal(new URL(launch.url).origin, appURLs[index]);
    const appPage = await context.newPage(); appPage.setDefaultTimeout(30000);
    await appPage.goto(launch.url);
    await appPage.getByRole('button', { name: `Sign in ${applications[index].name}`, exact: true }).click();
    await appPage.getByRole('heading', { name: `Private ${applications[index].name}`, exact: true }).waitFor();
    assert.equal(new URL(appPage.url()).origin, appURLs[index]);
    const cookies = await context.cookies(appURLs[index]);
    const auth = cookies.find(c => c.name === 'sim_auth');
    assert(auth?.httpOnly && auth.secure && auth.domain === new URL(appURLs[index]).hostname);
    const upload = await context.request.post(appURLs[index] + '/upload', { data: Buffer.alloc(1024 * 1024, 120), headers: { Origin: appURLs[index], 'Content-Type': 'application/octet-stream' } });
    assert.equal(upload.status(), 200); assert.equal(await upload.text(), String(1024 * 1024));
    appPages.push(appPage);
  }
  done(stage);
  stage = 'site authorization isolation and unauthenticated rejection';
  await api('/users', 'POST', { username: 'reader', password, siteIDs: [sites[0].id] });
  const member = await browser.newContext();
  const login = await member.request.post(consoleURL + '/api/login', { headers: { Origin: consoleURL }, data: { username: 'reader', password } });
  assert(login.ok());
  const visible = await api('/services', 'GET', undefined, member.request);
  assert.deepEqual(visible.map(s => s.id), [applications[0].id]);
  const denied = await member.request.post(consoleURL + `/api/services/${applications[1].id}/launch`, { headers: { Origin: consoleURL } });
  assert.equal(denied.status(), 404);
  assert.equal((await member.request.get(appURLs[1] + '/private')).status(), 401);
  done(stage);
  stage = 'Agent failure and public WSS reconnection';
  await stop(agents[0].process.child);
  await wait(async () => (await api('/agents')).filter(a => a.online).length === 3);
  await wait(async () => (await context.request.get(appURLs[0] + '/private')).status() === 200);
  start(binary, ['agent', '-c', agents[0].path], 'agent-reconnected', { TUNNEL_AGENT_TOKEN: agents[0].token });
  await wait(async () => (await api('/agents')).filter(a => a.online).length === 4);
  done(stage);
  stage = 'public bidirectional WebSocket and logout revocation';
  await appPages[0].evaluate(() => new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(Error('WebSocket timeout')), 20000);
    const socket = new WebSocket(location.origin.replace('https:', 'wss:') + '/ws');
    window.simulationSocketClosed = false;
    socket.onopen = () => socket.send('ping');
    socket.onmessage = event => { if (event.data === 'echo:ping') { clearTimeout(timer); resolve(true); } };
    socket.onerror = () => { clearTimeout(timer); reject(Error('WebSocket failed')); };
    socket.onclose = () => { window.simulationSocketClosed = true; };
    window.simulationSocket = socket;
  }));
  await api('/logout', 'POST');
  await wait(() => appPages[0].evaluate(() => window.simulationSocketClosed), 30);
  assert.equal((await context.request.get(appURLs[0] + '/private')).status(), 401);
  assert.equal((await member.request.get(consoleURL + '/api/me')).status(), 200);
  await member.close(); await context.close();
  done(stage);
  report.status = 'pass';
} catch (error) {
  report.status = 'fail'; report.failedStage = stage;
  // Do not copy browser errors containing one-time entry tickets into artifacts.
  report.errorType = error.name;
  console.error(`FAIL ${stage} (${error.name}); private temporary process logs are removed`);
  process.exitCode = 1;
} finally {
  if (browser) await browser.close();
  for (const child of children.reverse()) await stop(child);
  for (const socket of sockets) socket.destroy();
  for (const server of upstreams) await new Promise(r => server.close(r));
  for (const fd of logFDs) closeSync(fd);
  rmSync(directory, { recursive: true, force: true });
  report.cleanedUp = true;
  writeFileSync(resolve(output), JSON.stringify(report, null, 2) + '\n', { flag: 'wx', mode: 0o600 });
}
