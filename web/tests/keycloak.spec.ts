import { test, expect, type Page } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { createHash, createHmac, randomBytes } from "node:crypto";
import {
  mkdirSync,
  mkdtempSync,
  writeFileSync,
  rmSync,
  createWriteStream,
  copyFileSync,
  existsSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import http from "node:http";

const home = process.env.KEYCLOAK_HOME;
test.skip(
  !home,
  "Set KEYCLOAK_HOME to run the real Keycloak + OTP + Agent E2E suite",
);
test.setTimeout(180_000);
test.use({ actionTimeout: 15_000 });
const control = "http://localhost:19880",
  idp = "http://localhost:19881",
  realm = "tunnel-e2e";
const issuer = `${idp}/realms/${realm}`;
const password = "Fixture-" + randomBytes(16).toString("hex");
const clientSecret = randomBytes(24).toString("hex");
const syncSecret = randomBytes(24).toString("hex");
const subject = "2869bcad-a6aa-4ce1-84fc-4289cc356000";
let dir: string, importPath: string;
let keycloak: ChildProcess, server: ChildProcess, agent: ChildProcess;
let upstream: http.Server;
let adminCookie = "";
function start(
  command: string,
  args: string[],
  env: Record<string, string> = {},
  label = "child",
) {
  const child = spawn(command, args, {
    env: { ...process.env, ...env },
    stdio: ["ignore", "pipe", "pipe"],
  });
  const log = createWriteStream(resolve(dir, label + ".log"));
  child.stdout?.pipe(log);
  child.stderr?.pipe(log);
  child.on("error", (e) => console.error(label, e));
  return child;
}
async function stop(child?: ChildProcess) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  await new Promise<void>((done) => {
    const timer = setTimeout(() => child.kill("SIGKILL"), 10000);
    child.once("exit", () => {
      clearTimeout(timer);
      done();
    });
    child.kill("SIGTERM");
  });
}
async function api(path: string, method = "GET", body?: unknown) {
  const response = await fetch(control + "/api" + path, {
    method,
    headers: {
      "Content-Type": "application/json",
      Origin: control,
      Cookie: adminCookie,
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const result = await response.json();
  expect(response.ok, JSON.stringify(result)).toBe(true);
  return { response, result };
}
function otp(secret: string) {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
  let bits = "";
  for (const ch of secret.replace(/\s/g, "").toUpperCase()) {
    const value = alphabet.indexOf(ch);
    if (value < 0) throw Error("bad base32 secret");
    bits += value.toString(2).padStart(5, "0");
  }
  const key = Buffer.from(
    bits.match(/.{8}/g)!.map((byte) => parseInt(byte, 2)),
  );
  const step = Buffer.alloc(8);
  step.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)));
  const hash = createHmac("sha1", key).update(step).digest();
  const offset = hash[19] & 15;
  return ((hash.readUInt32BE(offset) & 0x7fffffff) % 1000000)
    .toString()
    .padStart(6, "0");
}
async function keycloakAdminToken() {
  const r = await fetch(idp + "/realms/master/protocol/openid-connect/token", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "password",
      client_id: "admin-cli",
      username: "admin",
      password,
    }),
  });
  expect(r.status).toBe(200);
  return (await r.json()).access_token as string;
}
async function configureSyncReader() {
  const token = await keycloakAdminToken();
  const base = idp + `/admin/realms/${realm}`;
  async function get(path: string) {
    const r = await fetch(base + path, {
      headers: { Authorization: "Bearer " + token },
    });
    expect(r.status).toBe(200);
    return r.json();
  }
  const serviceClient = (await get("/clients?clientId=tunnel-sync-e2e"))[0],
    realmClient = (await get("/clients?clientId=realm-management"))[0];
  const serviceUser = await get(
    `/clients/${serviceClient.id}/service-account-user`,
  );
  const roles = [];
  for (const role of ["view-users", "query-users"])
    roles.push(await get(`/clients/${realmClient.id}/roles/${role}`));
  const r = await fetch(
    base + `/users/${serviceUser.id}/role-mappings/clients/${realmClient.id}`,
    {
      method: "POST",
      headers: {
        Authorization: "Bearer " + token,
        "Content-Type": "application/json",
      },
      body: JSON.stringify(roles),
    },
  );
  expect(r.status).toBe(204);
}
async function passwordLogin(page: Page) {
  await page.getByRole("link", { name: "使用统一身份登录" }).click();
  await page.locator("#username").fill("operator");
  await page.locator("#password").fill(password);
  await page.locator("#kc-login").click();
}

test.beforeAll(async () => {
  if (!home) return;
  dir = mkdtempSync(resolve(tmpdir(), "tunnel-keycloak-e2e-"));
  const importDir = resolve(home, "data/import");
  mkdirSync(importDir, { recursive: true });
  const candidateImport = resolve(importDir, realm + "-realm.json");
  writeFileSync(
    candidateImport,
    JSON.stringify({
      realm,
      enabled: true,
      sslRequired: "none",
      registrationAllowed: false,
      accessTokenLifespan: 300,
      clients: [
        {
          clientId: "tunnel-e2e",
          enabled: true,
          protocol: "openid-connect",
          publicClient: false,
          secret: clientSecret,
          standardFlowEnabled: true,
          directAccessGrantsEnabled: false,
          redirectUris: [control + "/auth/oidc/callback"],
          frontchannelLogout: false,
          attributes: {
            "pkce.code.challenge.method": "S256",
            "backchannel.logout.url": control + "/auth/oidc/backchannel-logout",
            "backchannel.logout.session.required": "true",
            "post.logout.redirect.uris": control + "/",
          },
        },
        {
          clientId: "tunnel-sync-e2e",
          enabled: true,
          protocol: "openid-connect",
          publicClient: false,
          secret: syncSecret,
          serviceAccountsEnabled: true,
          standardFlowEnabled: false,
          directAccessGrantsEnabled: false,
        },
      ],
      users: [
        {
          id: subject,
          username: "operator",
          enabled: true,
          emailVerified: true,
          firstName: "E2E",
          lastName: "Operator",
          email: "operator@example.test",
          credentials: [
            { type: "password", value: password, temporary: false },
          ],
          requiredActions: ["CONFIGURE_TOTP"],
        },
      ],
    }),
    { mode: 0o600, flag: "wx" },
  );
  importPath = candidateImport;
  keycloak = start(
    resolve(home, "bin/kc.sh"),
    [
      "start-dev",
      "--http-host=127.0.0.1",
      "--http-port=19881",
      "--import-realm",
    ],
    {
      KC_BOOTSTRAP_ADMIN_USERNAME: "admin",
      KC_BOOTSTRAP_ADMIN_PASSWORD: password,
      KC_DB_URL: `jdbc:h2:file:${resolve(dir, "keycloakdb")};NON_KEYWORDS=VALUE;DB_CLOSE_ON_EXIT=FALSE`,
      JAVA_OPTS_APPEND: "-Xms128m -Xmx512m",
    },
    "keycloak",
  );
  await expect
    .poll(
      async () => {
        try {
          return (await fetch(issuer + "/.well-known/openid-configuration")).ok;
        } catch {
          return false;
        }
      },
      { timeout: 90_000 },
    )
    .toBe(true);
  await configureSyncReader();
  writeFileSync(
    resolve(dir, "server.json"),
    JSON.stringify({
      listen: "127.0.0.1:19880",
      publicURL: control,
      accessURL: "http://apps.localhost:19880",
      dataDir: resolve(dir, "data"),
      storage: process.env.TUNNEL_TEST_STORAGE || "json",
      oidc: {
        issuer,
        clientID: "tunnel-e2e",
        sessionMinutes: 5,
        keycloakSync: {
          enabled: true,
          clientID: "tunnel-sync-e2e",
          subjectIsUserID: true,
          pollSeconds: 1,
          maxStaleSeconds: 10,
        },
      },
    }),
  );
  server = start(
    resolve(process.cwd(), "../bin/tunnel"),
    ["server", "-c", resolve(dir, "server.json")],
    {
      TUNNEL_ADMIN_PASSWORD: password,
      TUNNEL_OIDC_CLIENT_SECRET: clientSecret,
      TUNNEL_KEYCLOAK_SYNC_CLIENT_SECRET: syncSecret,
    },
    "control",
  );
  await expect
    .poll(async () => {
      try {
        return (await fetch(control + "/healthz")).ok;
      } catch {
        return false;
      }
    })
    .toBe(true);
  const login = await api("/login", "POST", { username: "admin", password });
  adminCookie = login.response.headers
    .getSetCookie()
    .find((c) => c.startsWith("tunnel_console="))!
    .split(";")[0];
  upstream = http.createServer((req, res) => {
    if (req.url === "/events") {
      res.writeHead(200, {
        "Content-Type": "text/event-stream",
        "Cache-Control": "no-store",
      });
      res.write("data: ready\n\n");
      const heartbeat = setInterval(() => res.write(": heartbeat\n\n"), 500);
      req.on("close", () => clearInterval(heartbeat));
      return;
    }
    res.setHeader("Content-Type", "text/html");
    res.end("<!doctype html><h1>Real Keycloak authenticated application</h1>");
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
  await new Promise<void>((done) => upstream.listen(19882, "127.0.0.1", done));
});
test.afterAll(async () => {
  await stop(agent);
  await stop(server);
  await stop(keycloak);
  if (upstream) await new Promise<void>((done) => upstream.close(() => done()));
  if (importPath) rmSync(importPath, { force: true });
  if (dir) {
    if (test.info().status !== test.info().expectedStatus) {
      const target = test.info().outputPath("diagnostics");
      mkdirSync(target, { recursive: true });
      for (const name of ["keycloak.log", "control.log", "agent.log"]) {
        const source = resolve(dir, name);
        if (existsSync(source)) copyFileSync(source, resolve(target, name));
      }
      console.error("Keycloak E2E diagnostics copied to", target);
    } else rmSync(dir, { recursive: true, force: true });
  }
});

test("real Keycloak OTP, authorization, session revocation and back-channel logout", async ({
  page,
  browser,
}) => {
  // First authentication performs real OTP enrollment but has no local binding.
  await page.goto(control);
  await passwordLogin(page);
  await page.getByRole("link", { name: /Unable to scan/ }).click();
  const secret = (await page
    .locator("#kc-totp-secret-key")
    .textContent())!.replace(/\s/g, "");
  const enrollmentCode = otp(secret);
  await page.locator("#totp").fill(enrollmentCode);
  await page.locator("#userLabel").fill("E2E authenticator");
  await page.getByRole("button", { name: "Submit", exact: true }).click();
  await expect(page.locator(".form-error")).toContainText("统一登录失败");
  const site = await api("/sites", "POST", { name: "Keycloak E2E site" });
  const app = await api("/services", "POST", {
    name: "Real IdP app",
    siteID: site.result.site.id,
    url: "http://127.0.0.1:19882",
  });
  await api("/users", "POST", {
    username: "keycloak-member",
    oidcSubject: subject,
    serviceIDs: [app.result.id],
  });
  const agentConfig = resolve(dir, "agent.json");
  writeFileSync(
    agentConfig,
    JSON.stringify({
      serverURL: control,
      siteID: site.result.site.id,
      allow: [{ cidr: "127.0.0.0/8", ports: [19882] }],
    }),
  );
  agent = start(
    resolve(process.cwd(), "../bin/tunnel"),
    ["agent", "-c", agentConfig],
    { TUNNEL_AGENT_TOKEN: site.result.token },
    "agent",
  );
  await expect
    .poll(async () => {
      const status = await api("/sites");
      return status.result[0]?.online;
    })
    .toBe(true);
  // A fresh browser must now supply password AND the previously enrolled OTP.
  const context = await browser.newContext();
  const member = await context.newPage();
  try {
    await member.goto(control);
    await passwordLogin(member);
    await expect(member.locator("#otp")).toBeVisible();
    // Keycloak rejects reuse of the enrollment OTP within its current period.
    await expect
      .poll(() => otp(secret), { timeout: 35_000 })
      .not.toBe(enrollmentCode);
    const firstLoginCode = otp(secret);
    await member.locator("#otp").fill(firstLoginCode);
    await member.locator("#kc-login").click();
    await expect(member.locator(".profile")).toContainText("keycloak-member");
    await expect(member.getByRole("button", { name: "访问权限" })).toHaveCount(
      0,
    );
    await member.getByRole("button", { name: /^应用/ }).click();
    const popup = member.waitForEvent("popup");
    await member.getByRole("button", { name: "打开", exact: true }).click();
    const appPage = await popup;
    await expect(appPage.getByRole("heading")).toHaveText(
      "Real Keycloak authenticated application",
    );
    await appPage.evaluate(() => {
      const state = {
        wsReady: false,
        sseReady: false,
        wsClosed: false,
        sseClosed: false,
      };
      (window as unknown as { e2e: typeof state }).e2e = state;
      const socket = new WebSocket(
        location.origin.replace("http", "ws") + "/ws",
      );
      socket.onmessage = () => {
        state.wsReady = true;
      };
      socket.onclose = () => {
        state.wsClosed = true;
      };
      const events = new EventSource("/events");
      events.onmessage = () => {
        state.sseReady = true;
      };
      events.onerror = () => {
        state.sseClosed = true;
        events.close();
      };
    });
    await expect
      .poll(() =>
        appPage.evaluate(() => {
          const s = (
            window as unknown as {
              e2e: { wsReady: boolean; sseReady: boolean };
            }
          ).e2e;
          return s.wsReady && s.sseReady;
        }),
      )
      .toBe(true);
    const sessions = await api("/sessions");
    expect(
      sessions.result.some(
        (s: { username: string; method: string }) =>
          s.username === "keycloak-member" && s.method === "oidc",
      ),
    ).toBe(true);
    const tokenResponse = await fetch(
      idp + "/realms/master/protocol/openid-connect/token",
      {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: new URLSearchParams({
          grant_type: "password",
          client_id: "admin-cli",
          username: "admin",
          password,
        }),
      },
    );
    expect(tokenResponse.ok).toBe(true);
    const token = await tokenResponse.json();
    const logout = await fetch(
      idp + `/admin/realms/${realm}/users/${subject}/logout`,
      {
        method: "POST",
        headers: { Authorization: "Bearer " + token.access_token },
      },
    );
    expect(logout.status).toBe(204);
    await expect
      .poll(async () => {
        const me = await member.request.get(control + "/api/me");
        return me.status();
      })
      .toBe(401);
    await expect
      .poll(() =>
        appPage.evaluate(() => {
          const s = (
            window as unknown as {
              e2e: { wsClosed: boolean; sseClosed: boolean };
            }
          ).e2e;
          return s.wsClosed && s.sseClosed;
        }),
      )
      .toBe(true);
    await appPage.reload();
    await expect(appPage.locator("body")).not.toContainText(
      "Real Keycloak authenticated application",
    );
    await member.reload();
    await expect(
      member.getByRole("button", { name: "登录控制台", exact: true }),
    ).toBeVisible();
    // Exercise RP-initiated logout against the real provider too. It must clear
    // the local session before navigation and end the provider SSO session.
    await passwordLogin(member);
    await expect
      .poll(() => otp(secret), { timeout: 35_000 })
      .not.toBe(firstLoginCode);
    const secondLoginCode = otp(secret);
    await member.locator("#otp").fill(secondLoginCode);
    await member.locator("#kc-login").click();
    await expect(member.locator(".profile")).toContainText("keycloak-member");
    await member
      .getByRole("button", { name: "退出统一身份", exact: true })
      .click();
    await member.getByRole("button", { name: "Logout", exact: true }).click();
    await expect(
      member.getByRole("button", { name: "登录控制台", exact: true }),
    ).toBeVisible();
    await member.getByRole("link", { name: "使用统一身份登录" }).click();
    await expect(member.locator("#username")).toBeVisible();
    await member.screenshot({
      path: test.info().outputPath("keycloak-backchannel-logout.png"),
      fullPage: true,
    });
    // Disable the real IdP user without calling its logout endpoint. The local
    // status bridge must revoke console/application sessions and their streams.
    await member.locator("#username").fill("operator");
    await member.locator("#password").fill(password);
    await member.locator("#kc-login").click();
    await expect
      .poll(() => otp(secret), { timeout: 35000 })
      .not.toBe(secondLoginCode);
    await member.locator("#otp").fill(otp(secret));
    await member.locator("#kc-login").click();
    await expect(member.locator(".profile")).toContainText("keycloak-member");
    const newLaunch = await member.request.post(
      control + `/api/services/${app.result.id}/launch`,
      { headers: { Origin: control } },
    );
    expect(newLaunch.status()).toBe(200);
    await appPage.goto((await newLaunch.json()).url);
    await expect(appPage.getByRole("heading")).toHaveText(
      "Real Keycloak authenticated application",
    );
    await appPage.evaluate(() => {
      const w = window as any;
      w.sync = { ready: false, closed: false, wsReady: false, wsClosed: false };
      const socket = new WebSocket(
        location.origin.replace("http", "ws") + "/ws",
      );
      socket.onmessage = () => (w.sync.wsReady = true);
      socket.onclose = () => (w.sync.wsClosed = true);
      void (async () => {
        try {
          const r = await fetch("/events"),
            reader = r.body!.getReader();
          await reader.read();
          w.sync.ready = true;
          while (!(await reader.read()).done) {}
        } catch {
        } finally {
          w.sync.closed = true;
        }
      })();
    });
    await expect
      .poll(() =>
        appPage.evaluate(
          () => (window as any).sync.ready && (window as any).sync.wsReady,
        ),
      )
      .toBe(true);
    const disableToken = await keycloakAdminToken();
    const disable = await fetch(
      idp + `/admin/realms/${realm}/users/${subject}`,
      {
        method: "PUT",
        headers: {
          Authorization: "Bearer " + disableToken,
          "Content-Type": "application/json",
        },
        body: JSON.stringify({ enabled: false }),
      },
    );
    expect(disable.status).toBe(204);
    await expect
      .poll(
        async () => (await member.request.get(control + "/api/me")).status(),
        { timeout: 10000 },
      )
      .toBe(401);
    await expect
      .poll(() =>
        appPage.evaluate(
          () => (window as any).sync.closed && (window as any).sync.wsClosed,
        ),
      )
      .toBe(true);
    expect(await appPage.evaluate(async () => (await fetch("/")).status)).toBe(
      401,
    );
    const metrics = await fetch(control + "/api/metrics", {
      headers: { Cookie: adminCookie },
    });
    const text = await metrics.text();
    expect(
      Number(text.match(/tunnel_identity_sync_revocations_total (\d+)/)?.[1]),
    ).toBeGreaterThan(0);
    expect((await api("/me")).result.username).toBe("admin");
    const enable = await fetch(
      idp + `/admin/realms/${realm}/users/${subject}`,
      {
        method: "PUT",
        headers: {
          Authorization: "Bearer " + disableToken,
          "Content-Type": "application/json",
        },
        body: JSON.stringify({ enabled: true }),
      },
    );
    expect(enable.status).toBe(204);
    expect((await member.request.get(control + "/api/me")).status()).toBe(401);
  } finally {
    await context.close();
  }
});
