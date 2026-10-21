#!/usr/bin/env node
// Private synthetic-account browser acceptance; no credentials enter reports.
import { readFileSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { createRequire } from 'node:module';
import assert from 'node:assert/strict';

const require = createRequire(new URL('../web/package.json', import.meta.url));
const { chromium } = require('@playwright/test');
const runtime = resolve(process.argv[2]);
const read = name => JSON.parse(readFileSync(resolve(runtime, name), 'utf8'));
const binding = read('binding.json'), build = read('build.json'), credentials = read('credentials.json');
const { applications } = read('status.json');
const report = { buildID: build.buildID, source: build.source, origin: binding.origin,
  localMode: binding.local, generatedAt: new Date().toISOString(), stages: [], status: 'running' };
let browser, stage = 'browser startup';
const pass = name => report.stages.push({ name, result: 'pass' });
try {
  const proxyURL = !binding.local && (process.env.https_proxy || process.env.HTTPS_PROXY);
  const parsedProxy = proxyURL ? new URL(proxyURL) : null;
  if (parsedProxy) assert.equal(parsedProxy.protocol, 'http:');
  const proxy = parsedProxy ? { server: parsedProxy.origin,
    ...(parsedProxy.username ? { username: decodeURIComponent(parsedProxy.username),
      password: decodeURIComponent(parsedProxy.password) } : {}),
    bypass: process.env.no_proxy || process.env.NO_PROXY || '' } : undefined;
  browser = await chromium.launch(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
    ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : {});
  const context = await browser.newContext(proxy ? { proxy } : {});
  const page = await context.newPage();
  page.setDefaultTimeout(20000);
  stage = 'console login and embedded identity';
  await page.goto(binding.origin);
  await page.getByLabel('用户名', { exact: true }).fill(credentials.username);
  await page.getByLabel('密码', { exact: true }).fill(credentials.password);
  await page.getByRole('button', { name: '登录控制台', exact: true }).click();
  await page.locator('.profile').filter({ hasText: credentials.username }).waitFor();
  const identity = await (await context.request.get(binding.origin + '/__preview.json', { maxRetries: 2 })).json();
  assert.equal(identity.buildID, build.buildID);
  pass(stage);
  const appPages = [];
  for (const app of applications) {
    stage = `application ${app.name}: ticket, Agent HTTP and upload`;
    const response = await context.request.post(binding.origin + `/api/services/${app.id}/launch`,
      { headers: { Origin: binding.origin } });
    assert.equal(response.status(), 200);
    const { url } = await response.json();
    assert.equal(new URL(url).origin, app.url);
    const appPage = await context.newPage();
    await appPage.goto(url);
    await appPage.getByRole('heading', { name: `Preview application ${app.name}`, exact: true }).waitFor();
    assert((await appPage.textContent('body')).includes(build.buildID));
    const payload = Buffer.alloc(65536, 120);
    const upload = await context.request.post(app.url + '/upload',
      { data: payload, headers: { Origin: app.url, 'Content-Type': 'application/octet-stream' } });
    assert.equal(upload.status(), 200);
    assert.deepEqual(await upload.json(), { bytes: payload.length, buildID: build.buildID });
    pass(stage);
    stage = `application ${app.name}: SSE and WebSocket`;
    assert.equal(await appPage.evaluate(() => new Promise((resolve, reject) => {
      const source = new EventSource('/events');
      window.previewEvents = source;
      const timer = setTimeout(() => { source.close(); reject(Error('SSE timeout')); }, 10000);
      window.previewEventsClosed = new Promise(done => {
        source.onerror = () => {
          clearTimeout(timer);
          source.close(); // Prevent EventSource from reconnecting after revocation.
          done(true);
          reject(Error('SSE ended before its first event'));
        };
      });
      source.onmessage = event => { clearTimeout(timer); resolve(event.data); };
    })), 'preview-event');
    assert.equal(await appPage.evaluate(() => new Promise((resolve, reject) => {
      const ws = new WebSocket(location.origin.replace(/^http/, 'ws') + '/ws');
      window.previewSocket = ws;
      const timer = setTimeout(() => { ws.close(); reject(Error('WS timeout')); }, 10000);
      window.previewSocketClosed = new Promise(done => ws.addEventListener('close', () => done(true)));
      ws.onopen = () => ws.send('preview-echo');
      ws.onmessage = event => { clearTimeout(timer); resolve(event.data); };
      ws.onerror = () => { clearTimeout(timer); reject(Error('WS failure')); };
    })), 'preview-echo');
    pass(stage);
    appPages.push({ page: appPage, url: app.url });
  }
  stage = 'logout revokes both application sessions and active SSE/WebSockets';
  for (const app of appPages) {
    assert(await app.page.evaluate(() =>
      window.previewEvents.readyState === EventSource.OPEN && window.previewSocket.readyState === WebSocket.OPEN),
      'SSE and WebSocket must still be active before logout');
  }
  const logout = await context.request.post(binding.origin + '/api/logout', { headers: { Origin: binding.origin } });
  assert.equal(logout.status(), 200);
  for (const app of appPages) {
    assert(await app.page.evaluate(() => Promise.race([
      Promise.all([window.previewEventsClosed, window.previewSocketClosed]).then(() => true),
      new Promise(resolve => setTimeout(() => resolve(false), 10000)),
    ])), 'Logout must terminate the existing SSE and WebSocket connections');
    const denied = await context.request.get(app.url, { maxRetries: 2 });
    assert([401, 403].includes(denied.status()));
  }
  pass(stage);
  report.status = 'pass';
} catch (error) {
  report.status = 'fail';
  report.stages.push({ name: stage, result: 'fail' });
  writeFileSync(resolve(runtime, 'smoke-error.log'), String(error.stack ?? error), { mode: 0o600 });
  console.error('Preview browser smoke failed at: ' + stage);
  process.exitCode = 1;
} finally {
  await browser?.close();
  writeFileSync(resolve(runtime, 'smoke-report.json'), JSON.stringify(report, null, 2) + '\n', { mode: 0o600 });
  console.log(JSON.stringify(report, null, 2));
}
