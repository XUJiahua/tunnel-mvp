import { test, expect } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { createHash, generateKeyPairSync, randomUUID, sign } from "node:crypto";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import http from "node:http";

const consoleURL = "http://localhost:19080";
const issuer = "http://localhost:19081";
const password = "browser-oidc-admin-password";
let dir: string;
let server: ChildProcess;
let provider: http.Server;
const { privateKey, publicKey } = generateKeyPairSync("rsa", {
  modulusLength: 2048,
});
const codes = new Map<string, URLSearchParams>();
const json = (res: http.ServerResponse, value: unknown) => {
  res.writeHead(200, {
    "Content-Type": "application/json",
    "Cache-Control": "no-store",
  });
  res.end(JSON.stringify(value));
};

test.beforeAll(async () => {
  provider = http.createServer(async (req, res) => {
    const url = new URL(req.url || "/", issuer);
    if (url.pathname === "/.well-known/openid-configuration") {
      json(res, {
        issuer,
        authorization_endpoint: issuer + "/authorize",
        token_endpoint: issuer + "/token",
        jwks_uri: issuer + "/keys",
        id_token_signing_alg_values_supported: ["RS256"],
      });
    } else if (url.pathname === "/keys") {
      json(res, {
        keys: [
          {
            ...publicKey.export({ format: "jwk" }),
            kid: "test",
            alg: "RS256",
            use: "sig",
          },
        ],
      });
    } else if (url.pathname === "/authorize") {
      const code = randomUUID();
      codes.set(code, url.searchParams);
      // A separate origin and an explicit user action exercise browser cookies
      // and SameSite=Lax on the real top-level callback navigation.
      res.setHeader("Content-Type", "text/html");
      res.end(
        `<h1>Test identity provider</h1><form action="/approve" method="post"><input type="hidden" name="code" value="${code}"><button>Continue as external member</button></form>`,
      );
    } else if (url.pathname === "/approve" || url.pathname === "/token") {
      let raw = "";
      for await (const chunk of req) raw += chunk;
      const body = new URLSearchParams(raw);
      const code = body.get("code") || "";
      const auth = codes.get(code);
      if (!auth) {
        res.writeHead(400);
        res.end();
        return;
      }
      if (url.pathname === "/approve") {
        const dest = new URL(auth.get("redirect_uri")!);
        dest.searchParams.set("code", code);
        dest.searchParams.set("state", auth.get("state")!);
        res.writeHead(302, { Location: dest.href });
        res.end();
        return;
      }
      codes.delete(code);
      const challenge = createHash("sha256")
        .update(body.get("code_verifier") || "")
        .digest("base64url");
      if (
        challenge !== auth.get("code_challenge") ||
        auth.get("code_challenge_method") !== "S256" ||
        auth.get("scope") !== "openid" ||
        req.headers.authorization !==
          "Basic " +
            Buffer.from("browser:browser-oidc-secret").toString("base64") ||
        body.get("redirect_uri") !== consoleURL + "/auth/oidc/callback"
      ) {
        res.writeHead(400);
        res.end();
        return;
      }
      const header = Buffer.from(
        JSON.stringify({ alg: "RS256", kid: "test" }),
      ).toString("base64url");
      const payload = Buffer.from(
        JSON.stringify({
          iss: issuer,
          sub: "external-subject",
          aud: "browser",
          exp: Math.floor(Date.now() / 1000) + 600,
          nonce: auth.get("nonce"),
          email: "admin",
          roles: ["admin"],
        }),
      ).toString("base64url");
      const unsigned = `${header}.${payload}`;
      json(res, {
        access_token: "private-test-token",
        token_type: "Bearer",
        id_token: `${unsigned}.${sign("RSA-SHA256", Buffer.from(unsigned), privateKey).toString("base64url")}`,
      });
    } else {
      res.writeHead(404);
      res.end();
    }
  });
  await new Promise<void>((done) => provider.listen(19081, "127.0.0.1", done));
  dir = mkdtempSync(resolve(tmpdir(), "tunnel-oidc-e2e-"));
  writeFileSync(
    resolve(dir, "server.json"),
    JSON.stringify({
      listen: "127.0.0.1:19080",
      publicURL: consoleURL,
      accessURL: "http://apps.localhost:19080",
      dataDir: resolve(dir, "data"),
      storage: process.env.TUNNEL_TEST_STORAGE || "json",
      oidc: { issuer, clientID: "browser", sessionMinutes: 10 },
    }),
  );
  server = spawn(
    resolve(process.cwd(), "../bin/tunnel"),
    ["server", "--config", resolve(dir, "server.json")],
    {
      env: {
        ...process.env,
        TUNNEL_ADMIN_PASSWORD: password,
        TUNNEL_OIDC_CLIENT_SECRET: "browser-oidc-secret",
      },
      stdio: "ignore",
    },
  );
  await expect
    .poll(async () => {
      try {
        return (await fetch(consoleURL + "/healthz")).ok;
      } catch {
        return false;
      }
    })
    .toBe(true);
});

test.afterAll(async () => {
  if (server && server.exitCode === null && server.signalCode === null)
    await new Promise<void>((done) => {
      server.once("exit", () => done());
      server.kill("SIGTERM");
    });
  if (provider) await new Promise<void>((done) => provider.close(() => done()));
  if (dir) rmSync(dir, { recursive: true, force: true });
});

test("browser OIDC binding, account disable and local recovery login", async ({
  page,
  browser,
}) => {
  const context = await browser.newContext();
  const member = await context.newPage();
  try {
    await member.goto(consoleURL);
    await member.getByRole("link", { name: "使用统一身份登录" }).click();
    await member
      .getByRole("button", { name: "Continue as external member" })
      .click();
    await expect(member.locator(".form-error")).toContainText("统一登录失败");

    await page.goto(consoleURL);
    await page.getByLabel("用户名", { exact: true }).fill("admin");
    await page.getByLabel("密码", { exact: true }).fill(password);
    await page.getByRole("button", { name: "登录控制台" }).click();
    await page.getByRole("button", { name: "访问权限" }).click();
    await page.getByRole("button", { name: "添加成员", exact: true }).click();
    await page.getByLabel("用户名", { exact: true }).fill("external");
    await page.getByLabel("登录方式").selectOption("oidc");
    await page.getByLabel("OIDC Subject").fill("external-subject");
    await expect(page.getByLabel("初始密码")).toHaveCount(0);
    await page.getByRole("button", { name: "创建", exact: true }).click();
    const row = page.getByRole("row").filter({ hasText: "external" });
    await expect(row).toContainText("OIDC");
    await expect(row.getByRole("button", { name: "重置密码" })).toHaveCount(0);

    await member.getByRole("link", { name: "使用统一身份登录" }).click();
    await member
      .getByRole("button", { name: "Continue as external member" })
      .click();
    await expect(member.locator(".profile")).toContainText("external");
    await expect(member.getByRole("button", { name: "访问权限" })).toHaveCount(
      0,
    );
    await expect(member.getByRole("button", { name: "修改密码" })).toHaveCount(
      0,
    );
    const cookies = await context.cookies();
    expect(
      cookies.some(
        (cookie) => cookie.name === "tunnel_console" && cookie.httpOnly,
      ),
    ).toBe(true);
    expect(cookies.some((cookie) => cookie.name === "tunnel_oidc")).toBe(false);

    page.once("dialog", (dialog) => dialog.accept());
    await row.getByRole("button", { name: "禁用账号" }).click();
    await expect(row).toContainText("已禁用");
    await member.reload();
    await expect(
      member.getByRole("button", { name: "登录控制台" }),
    ).toBeVisible();
    await member.getByRole("link", { name: "使用统一身份登录" }).click();
    await member
      .getByRole("button", { name: "Continue as external member" })
      .click();
    await expect(member.locator(".form-error")).toContainText("统一登录失败");

    await row.getByRole("button", { name: "启用账号" }).click();
    await row.getByRole("button", { name: "身份绑定" }).click();
    await page.getByLabel("OIDC Subject").fill("replacement-subject");
    await page.getByRole("button", { name: "保存", exact: true }).click();
    await expect(page.getByRole("dialog")).toHaveCount(0);
    await member.getByRole("link", { name: "使用统一身份登录" }).click();
    await member
      .getByRole("button", { name: "Continue as external member" })
      .click();
    await expect(member.locator(".form-error")).toContainText("统一登录失败");
    await expect(page.locator(".profile")).toContainText("admin");
    await page.screenshot({
      path: "test-results/oidc-members.png",
      fullPage: true,
    });
  } finally {
    await context.close();
  }
});
