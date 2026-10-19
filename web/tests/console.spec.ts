import { test, expect } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import http from "node:http";

const root = resolve(process.cwd(), "..");
const consoleURL = "http://localhost:18080";
const password = "browser-fixture-password-123";
let dir: string;
let server: ChildProcess;
let agent: ChildProcess | undefined;
let upstream: http.Server;
const leakedCookies: string[] = [];

function start(args: string[], env: Record<string, string>): ChildProcess {
  const child = spawn(resolve(root, "bin/tunnel"), args, {
    env: { ...process.env, ...env },
    stdio: "ignore",
  });
  child.on("error", (error) => console.error(error));
  return child;
}
async function stop(child?: ChildProcess) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  await new Promise<void>((done) => {
    child.once("exit", () => done());
    child.kill("SIGTERM");
  });
}

test.beforeAll(async () => {
  dir = mkdtempSync(resolve(tmpdir(), "tunnel-e2e-"));
  writeFileSync(
    resolve(dir, "server.json"),
    JSON.stringify({
      listen: "127.0.0.1:18080",
      storage: process.env.TUNNEL_TEST_STORAGE || "json",
      publicURL: consoleURL,
      accessURL: "http://apps.localhost:18080",
      dataDir: resolve(dir, "data"),
    }),
  );
  server = start(["server", "-config", resolve(dir, "server.json")], {
    TUNNEL_ADMIN_PASSWORD: password,
  });
  await expect
    .poll(async () => {
      try {
        return (await fetch(consoleURL + "/healthz")).ok;
      } catch {
        return false;
      }
    })
    .toBe(true);
  upstream = http.createServer((req, res) => {
    const cookie = req.headers.cookie || "";
    if (cookie.includes("tunnel_access") || cookie.includes("tunnel_console"))
      leakedCookies.push(cookie);
    if (req.url === "/login" && req.method === "POST") {
      res.writeHead(302, {
        "Set-Cookie":
          "demo_session=authenticated; Domain=internal.example; Path=/; HttpOnly",
        Location: "http://127.0.0.1:3000/",
      });
      res.end();
      return;
    }
    if (cookie.includes("demo_session=authenticated")) {
      res.end("<!doctype html><h1>Internal platform dashboard</h1>");
      return;
    }
    if (req.url !== "/login") {
      res.writeHead(302, { Location: "http://127.0.0.1:3000/login" });
      res.end();
      return;
    }
    res.end(
      '<!doctype html><h1>Internal platform sign in</h1><form action="/login" method="post"><label>Account<input name="account"></label><button>Sign in</button></form>',
    );
  });
  await new Promise<void>((done) => upstream.listen(3000, "127.0.0.1", done));
});

test.afterAll(async () => {
  await stop(agent);
  await stop(server);
  if (upstream) await new Promise<void>((done) => upstream.close(() => done()));
  if (dir) rmSync(dir, { recursive: true, force: true });
});

test("enroll site, open and log in to internal app, enforce viewer permissions", async ({
  page,
  browser,
}) => {
  const pageErrors: string[] = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  await page.goto(consoleURL);
  await page.getByLabel("用户名", { exact: true }).fill("admin");
  await page.getByLabel("密码", { exact: true }).fill(password);
  await page.getByRole("button", { name: "登录控制台" }).click();
  await page.getByRole("button", { name: "接入新站点" }).click();
  await page.getByLabel("站点名称").fill("上海数据中心");
  await page.getByRole("button", { name: "创建", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "上海数据中心 · Agent 接入" }),
  ).toBeVisible();
  const siteID = await page.locator(".token-value").nth(0).textContent();
  const token = await page.locator(".token-value").nth(1).textContent();
  if (!siteID || !token) throw new Error("Missing Agent credentials");
  await page.getByRole("button", { name: "已安全保存" }).click();
  writeFileSync(
    resolve(dir, "agent.json"),
    JSON.stringify({
      serverURL: consoleURL,
      siteID,
      allow: [{ cidr: "127.0.0.0/8", ports: [3000] }],
    }),
  );
  agent = start(["agent", "-config", resolve(dir, "agent.json")], {
    TUNNEL_AGENT_TOKEN: token,
  });
  await expect
    .poll(async () =>
      page.evaluate(async () => {
        const sites: { online: boolean }[] = await (
          await fetch("/api/sites")
        ).json();
        return sites.some((site) => site.online);
      }),
    )
    .toBe(true);
  await page.getByRole("button", { name: "刷新数据" }).click();

  await page.getByRole("button", { name: "接入新站点" }).click();
  await page.getByLabel("站点名称").fill("北京研发环境");
  await page.getByRole("button", { name: "创建", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "北京研发环境 · Agent 接入" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "已安全保存" }).click();
  await page.getByRole("button", { name: "添加应用", exact: true }).click();
  await page.getByLabel("应用名称").fill("数据面控制台");
  await page.getByLabel("所属站点").selectOption(siteID);
  await page.getByLabel("内网地址").fill("http://127.0.0.1:3000");
  await page.getByRole("button", { name: "创建", exact: true }).click();
  await page.getByRole("button", { name: "检测连接", exact: true }).click();
  await expect(page.getByRole("button", { name: /HTTP 302/ })).toBeVisible();

  const popupPromise = page.waitForEvent("popup");
  await page.getByRole("button", { name: "打开", exact: true }).click();
  const app = await popupPromise;
  await expect(
    app.getByRole("heading", { name: "Internal platform sign in" }),
  ).toBeVisible();
  expect(app.url()).toContain(".apps.localhost:18080/login");
  await app.getByLabel("Account").fill("john");
  await app.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(
    app.getByRole("heading", { name: "Internal platform dashboard" }),
  ).toBeVisible();
  expect(app.url()).not.toContain("127.0.0.1");
  expect(leakedCookies).toEqual([]);
  await app.close();

  // All processes are local and the site is online, but port 3001 is outside
  // the Agent ACL. The browser must show the actual failure and both trace IDs.
  await page.getByRole("button", { name: "添加应用", exact: true }).click();
  await page.getByLabel("应用名称").fill("白名单诊断");
  await page.getByLabel("所属站点").selectOption(siteID);
  await page.getByLabel("内网地址").fill("http://127.0.0.1:3001");
  await page.getByRole("button", { name: "创建", exact: true }).click();
  const failedRow = page
    .getByRole("row")
    .filter({ has: page.getByText("白名单诊断", { exact: true }) });
  await failedRow
    .getByRole("button", { name: "检测连接", exact: true })
    .click();
  const diagnosis = page.getByRole("dialog");
  await expect(
    diagnosis.getByRole("heading", { name: "连接诊断 · 白名单诊断" }),
  ).toBeVisible();
  await expect(diagnosis.locator(".diagnostic-status")).toContainText(
    "acl_denied",
  );
  await expect(
    diagnosis.getByText("Agent 请求 ID", { exact: true }),
  ).toBeVisible();
  await expect(
    diagnosis.getByText("Server 请求 ID", { exact: true }),
  ).toBeVisible();
  await expect(diagnosis.getByRole("alert")).toContainText("allow list");
  await expect(diagnosis.locator(".diagnostic-attempts")).toContainText(
    "127.0.0.1",
  );
  const retry = page.waitForResponse(
    (response) =>
      response.url().endsWith("/probe") &&
      response.request().method() === "POST",
  );
  await diagnosis.getByRole("button", { name: "重试", exact: true }).click();
  expect((await (await retry).json()).code).toBe("acl_denied");
  await page.screenshot({
    path: test.info().outputPath("diagnostic.png"),
    fullPage: true,
  });
  await diagnosis.getByRole("button", { name: "关闭", exact: true }).click();
  await failedRow
    .getByRole("button", { name: "诊断详情", exact: true })
    .click();
  await expect(
    diagnosis.getByRole("heading", { name: "连接诊断 · 白名单诊断" }),
  ).toBeVisible();
  await diagnosis.getByRole("button", { name: "关闭", exact: true }).click();

  await page.getByRole("button", { name: "访问权限", exact: true }).click();
  await page.getByRole("button", { name: "添加成员", exact: true }).click();
  await page.getByLabel("用户名", { exact: true }).fill("reader");
  await page.getByLabel("初始密码").fill(password);
  await page.getByLabel("上海数据中心", { exact: true }).check();
  await page.getByRole("button", { name: "创建", exact: true }).click();
  await expect(page.getByText("reader", { exact: true })).toBeVisible();

  const viewerContext = await browser.newContext();
  const viewer = await viewerContext.newPage();
  await viewer.goto(consoleURL);
  await viewer.getByLabel("用户名", { exact: true }).fill("reader");
  await viewer.getByLabel("密码", { exact: true }).fill(password);
  await viewer.getByRole("button", { name: "登录控制台" }).click();
  await expect(
    viewer.getByRole("heading", { name: "上海数据中心", exact: true }),
  ).toBeVisible();
  await expect(
    viewer.getByRole("heading", { name: "北京研发环境", exact: true }),
  ).toHaveCount(0);
  await expect(
    viewer.getByRole("button", { name: "访问权限", exact: true }),
  ).toHaveCount(0);
  // Resource edits retain identity and application-only grants hide siblings.
  const memberRow = page
    .getByRole("row")
    .filter({ has: page.getByText("reader", { exact: true }) });
  await memberRow
    .getByRole("button", { name: "编辑权限", exact: true })
    .click();
  await page.getByLabel("上海数据中心", { exact: true }).uncheck();
  await page.getByLabel("数据面控制台 · 上海数据中心", { exact: true }).check();
  await page.getByRole("button", { name: "保存", exact: true }).click();
  await viewer.getByRole("button", { name: "刷新数据", exact: true }).click();
  await viewer.getByRole("button", { name: /^应用/ }).click();
  await expect(
    viewer.getByRole("cell", { name: "数据面控制台", exact: true }),
  ).toBeVisible();
  await expect(
    viewer.getByRole("cell", { name: "白名单诊断", exact: true }),
  ).toHaveCount(0);
  await expect(
    viewer.getByRole("button", { name: "审计日志", exact: true }),
  ).toHaveCount(0);

  await page.getByRole("button", { name: /^站点/ }).first().click();
  const siteCard = page.locator(".site-card").filter({
    has: page.getByRole("heading", { name: "上海数据中心", exact: true }),
  });
  await siteCard.getByRole("button", { name: "编辑", exact: true }).click();
  await page.getByLabel("站点名称", { exact: true }).fill("上海生产环境");
  await page.getByRole("button", { name: "保存", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "上海生产环境", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: /^应用/ }).first().click();
  const editable = page.getByRole("row").filter({
    has: page.getByRole("cell", { name: "数据面控制台", exact: true }),
  });
  await editable.getByRole("button", { name: "编辑", exact: true }).click();
  await page.getByLabel("应用名称", { exact: true }).fill("生产控制台");
  await page
    .getByLabel("内网地址", { exact: true })
    .fill("http://127.0.0.1:3001");
  await page.getByRole("button", { name: "保存", exact: true }).click();
  await expect(
    page.getByRole("cell", { name: "生产控制台", exact: true }),
  ).toBeVisible();

  const updatedRow = page.getByRole("row").filter({
    has: page.getByRole("cell", { name: "生产控制台", exact: true }),
  });
  await expect(
    updatedRow.getByRole("button", { name: "检测连接", exact: true }),
  ).toBeVisible();

  await page.getByRole("button", { name: "审计日志", exact: true }).click();
  await page.getByLabel("事件类型", { exact: true }).fill("service_updated");
  await page.getByRole("button", { name: "查询审计", exact: true }).click();
  await expect(
    page.getByRole("cell", { name: "service_updated", exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: test.info().outputPath("audit.png"),
    fullPage: true,
  });

  await viewer.getByRole("button", { name: "修改密码", exact: true }).click();
  await viewer.getByLabel("当前密码", { exact: true }).fill(password);
  await viewer
    .getByLabel("新密码", { exact: true })
    .fill("viewer-changed-password-123");
  await viewer
    .getByLabel("确认新密码", { exact: true })
    .fill("viewer-changed-password-123");
  await viewer.getByRole("button", { name: "保存", exact: true }).click();
  await expect(
    viewer.getByRole("button", { name: "登录控制台", exact: true }),
  ).toBeVisible();
  await viewer.getByLabel("用户名", { exact: true }).fill("reader");
  await viewer
    .getByLabel("密码", { exact: true })
    .fill("viewer-changed-password-123");
  await viewer.getByRole("button", { name: "登录控制台", exact: true }).click();
  await expect(
    viewer.getByRole("heading", { name: "上海生产环境", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "访问权限", exact: true }).click();
  await memberRow
    .getByRole("button", { name: "重置密码", exact: true })
    .click();
  await page.getByLabel("新密码", { exact: true }).fill(password);
  await page.getByRole("button", { name: "保存", exact: true }).click();
  // A click does not await the async save. Wait for successful persistence and
  // revocation before checking a different browser context's old session.
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await viewer.getByRole("button", { name: "刷新数据", exact: true }).click();
  await expect(
    viewer.getByRole("button", { name: "登录控制台", exact: true }),
  ).toBeVisible();
  await viewerContext.close();

  await page.getByRole("button", { name: "概览", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "让每个内网，触手可及。" }),
  ).toBeVisible();
  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
  expect(pageErrors).toEqual([]);
});
