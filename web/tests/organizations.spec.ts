import { test, expect } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import http from "node:http";
const root = resolve(process.cwd(), ".."),
  url = "http://localhost:23880",
  password = "organization-e2e-password-123";
let dir: string,
  server: ChildProcess,
  agent: ChildProcess,
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
  dir = mkdtempSync(resolve(tmpdir(), "tunnel-organization-e2e-"));
  writeFileSync(
    resolve(dir, "server.json"),
    JSON.stringify({
      listen: "127.0.0.1:23880",
      publicURL: url,
      accessURL: "http://apps.localhost:23880",
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
  upstream = http.createServer((_req, res) =>
    res.end("<!doctype html><h1>Organization A application</h1>"),
  );
  await new Promise<void>((done) => upstream.listen(23882, "127.0.0.1", done));
});
test.afterAll(async () => {
  await stop(agent);
  await stop(server);
  if (upstream) await new Promise<void>((done) => upstream.close(() => done()));
  if (dir) rmSync(dir, { recursive: true, force: true });
});

test("organization UI, scoped operator, real Agent access and role withdrawal", async ({
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
  await page.getByRole("button", { name: "组织与角色", exact: true }).click();
  for (const name of ["Organization A", "Organization B"]) {
    await page.getByLabel("组织名称", { exact: true }).fill(name);
    await page.getByRole("button", { name: "创建组织", exact: true }).click();
    await expect(page.getByRole("cell", { name, exact: true })).toBeVisible();
  }
  const orgs = await (
      await page.request.get(url + "/api/organizations")
    ).json(),
    a = orgs.find((o: { name: string }) => o.name === "Organization A").id,
    b = orgs.find((o: { name: string }) => o.name === "Organization B").id;
  async function api(path: string, body: unknown) {
    const r = await page.request.post(url + "/api" + path, {
      headers: { Origin: url },
      data: body,
    });
    expect(r.ok(), await r.text()).toBe(true);
    return r.json();
  }
  const sa = await api("/sites", { name: "A site", organizationID: a }),
    sb = await api("/sites", { name: "B site", organizationID: b });
  const app = await api("/services", {
      name: "A app",
      siteID: sa.site.id,
      url: "http://127.0.0.1:23882",
    }),
    other = await api("/services", {
      name: "B app",
      siteID: sb.site.id,
      url: "http://127.0.0.1:23882",
    });
  await api("/users", { username: "operator-a", password, organizationID: a });
  await api("/users", { username: "member-b", password, organizationID: b });
  writeFileSync(
    resolve(dir, "agent.json"),
    JSON.stringify({
      serverURL: url,
      siteID: sa.site.id,
      allow: [{ cidr: "127.0.0.0/8", ports: [23882] }],
    }),
  );
  agent = start(["agent", "-c", resolve(dir, "agent.json")], {
    TUNNEL_AGENT_TOKEN: sa.token,
  });
  await expect
    .poll(async () => {
      const r = await page.request.get(url + "/api/sites");
      return (await r.json()).find((s: { id: string }) => s.id === sa.site.id)
        ?.online;
    })
    .toBe(true);
  // Remount to load API-created members, then configure the role through UI.
  await page.getByRole("button", { name: /^应用/ }).click();
  await page.getByRole("button", { name: "组织与角色", exact: true }).click();
  await page.getByLabel("角色所属组织", { exact: true }).selectOption(a);
  await page.getByLabel("角色名称", { exact: true }).fill("A operator");
  const capabilities = [
    "站点、令牌与 Agent 管理",
    "应用管理与诊断",
    "普通成员、密码与授权管理",
    "用户组与邀请管理",
    "组织审计查询",
    "访问组织内全部应用",
  ];
  for (const name of capabilities)
    await page.getByRole("checkbox", { name, exact: true }).check();
  await page.getByRole("button", { name: "创建角色", exact: true }).click();
  await expect(
    page
      .getByRole("cell")
      .filter({
        has: page.locator("strong").filter({ hasText: /^A operator$/ }),
      }),
  ).toBeVisible();
  page.once("dialog", (d) => d.accept());
  await page
    .getByLabel("角色 · operator-a", { exact: true })
    .selectOption({ label: "A operator" });
  await expect(
    page.getByLabel("角色 · operator-a", { exact: true }),
  ).not.toHaveValue("");
  await page.getByLabel("组织范围", { exact: true }).selectOption(a);
  await page.getByRole("button", { name: /^应用/ }).click();
  await expect(
    page.getByRole("cell", { name: "A app", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "B app", exact: true }),
  ).toHaveCount(0);
  const context = await browser.newContext(),
    operator = await context.newPage();
  try {
    await login(operator, "operator-a");
    await expect(
      operator.getByRole("button", { name: "组织与角色", exact: true }),
    ).toHaveCount(0);
    await expect(operator.getByLabel("组织范围", { exact: true })).toHaveCount(
      0,
    );
    await expect(
      operator.getByRole("button", { name: "访问权限", exact: true }),
    ).toBeVisible();
    for (const path of [
      "/services",
      "/sites",
      "/users",
      "/groups",
      "/invitations",
      "/organizations",
      "/roles",
    ]) {
      const r = await operator.request.get(url + "/api" + path);
      expect(r.status()).toBe(200);
      const text = await r.text();
      expect(text).not.toContain(b);
      expect(text).not.toContain(other.id);
      expect(text).not.toContain("member-b");
    }
    for (const path of ["/sessions", "/metrics"]) {
      expect((await operator.request.get(url + "/api" + path)).status()).toBe(
        403,
      );
    }
    expect(
      (
        await operator.request.post(url + `/api/services/${other.id}/launch`, {
          headers: { Origin: url },
        })
      ).status(),
    ).toBe(404);
    expect(
      (
        await operator.request.patch(url + `/api/sites/${sb.site.id}`, {
          headers: { Origin: url },
          data: { name: "stolen" },
        })
      ).ok(),
    ).toBe(false);
    await operator.getByRole("button", { name: /^应用/ }).click();
    const popup = operator.waitForEvent("popup");
    await operator.getByRole("button", { name: "打开", exact: true }).click();
    const appPage = await popup;
    await expect(appPage.getByRole("heading")).toHaveText(
      "Organization A application",
    );
    // A scoped manager can create an ordinary account, never assign management roles.
    await operator
      .getByRole("button", { name: "访问权限", exact: true })
      .click();
    await operator
      .getByRole("button", { name: "添加成员", exact: true })
      .click();
    const dialog = operator.getByRole("dialog");
    await dialog.getByLabel("用户名", { exact: true }).fill("created-in-a");
    await dialog.getByLabel("初始密码").fill(password);
    await dialog.getByRole("button", { name: "创建", exact: true }).click();
    await expect(dialog).toHaveCount(0);
    const users = await (await operator.request.get(url + "/api/users")).json();
    expect(
      users.find((u: { username: string }) => u.username === "created-in-a")
        .organizationID,
    ).toBe(a);
    await page.getByRole("button", { name: "组织与角色", exact: true }).click();
    const row = page
      .getByRole("row")
      .filter({
        has: page.locator("strong").filter({ hasText: /^A operator$/ }),
      });
    await row.getByRole("button", { name: "编辑角色", exact: true }).click();
    for (const name of capabilities.filter((n) => n !== "应用管理与诊断"))
      await page.getByRole("checkbox", { name, exact: true }).uncheck();
    await page.getByRole("button", { name: "保存角色", exact: true }).click();
    await expect(row).not.toContainText("访问组织内全部应用");
    expect((await operator.request.get(url + "/api/me")).status()).toBe(401);
    await appPage.reload();
    await expect(appPage.locator("body")).not.toContainText(
      "Organization A application",
    );
    await login(operator, "operator-a");
    await expect(
      operator.getByRole("button", { name: "访问权限", exact: true }),
    ).toHaveCount(0);
    await operator.getByRole("button", { name: /^应用/ }).click();
    await expect(
      operator.getByRole("button", { name: "打开", exact: true }),
    ).toBeDisabled();
    expect(
      (
        await operator.request.post(url + `/api/services/${app.id}/launch`, {
          headers: { Origin: url },
        })
      ).status(),
    ).toBe(404);
    await operator.getByRole("button", { name: "编辑", exact: true }).click();
    const editor = operator.getByRole("dialog");
    await editor.getByLabel("应用名称", { exact: true }).fill("A app edited");
    await editor.getByRole("button", { name: "保存", exact: true }).click();
    await expect(editor).toHaveCount(0);
    page.once("dialog", (d) => d.accept());
    await row.getByRole("button", { name: "删除角色", exact: true }).click();
    await expect(row).toHaveCount(0);
    expect((await operator.request.get(url + "/api/me")).status()).toBe(401);
    await page.setViewportSize({ width: 390, height: 844 });
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
  } finally {
    await context.close();
  }
});
