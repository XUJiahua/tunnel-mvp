import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import ts from "typescript";
const source = readFileSync(new URL("../app/api.ts", import.meta.url), "utf8");
const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2020, module: ts.ModuleKind.ESNext } }).outputText;
const { api, APIError } = await import(`data:text/javascript;base64,${Buffer.from(js).toString("base64")}`);
function reply(data, cursor, status = 200) {
  return new Response(JSON.stringify(data), { status, headers: cursor ? { "X-Tunnel-Next-Cursor": cursor } : {} });
}

test("complete directory and roles retain existing response shapes", async () => {
  const calls = [];
  globalThis.fetch = async (url) => { calls.push(url); return url.includes("cursor=") ? reply([{ id: "second" }]) : reply([{ id: "first" }], "page+2"); };
  assert.deepEqual(await api("/sites"), [{ id: "first" }, { id: "second" }]);
  assert.deepEqual(calls, ["/api/sites?limit=100", "/api/sites?limit=100&cursor=page%2B2"]);
  globalThis.fetch = async (url) => reply({ roles: [{ id: url.includes("cursor=") ? "second" : "first" }], permissions: ["audit:read"] }, url.includes("cursor=") ? null : "next");
  assert.deepEqual(await api("/roles"), { roles: [{ id: "first" }, { id: "second" }], permissions: ["audit:read"] });
});
test("revision conflict discards partial pages and retries the whole collection once", async () => {
  const responses = [reply([{ id: "stale" }], "old"), reply({ error: "changed" }, null, 409), reply([{ id: "fresh" }], "new"), reply([{ id: "last" }])];
  globalThis.fetch = async () => responses.shift();
  assert.deepEqual(await api("/users"), [{ id: "fresh" }, { id: "last" }]);
  assert.equal(responses.length, 0);
  let calls = 0;
  globalThis.fetch = async () => { calls++; return reply({ error: "changed" }, null, 409); };
  await assert.rejects(api("/users"), (error) => error instanceof APIError && error.status === 409);
  assert.equal(calls, 2);
});
test("transient failures return no partial collection and writes are sent once", async () => {
  let calls = 0;
  globalThis.fetch = async () => ++calls === 1 ? reply([{ id: "partial" }], "next") : reply({ error: "unavailable" }, null, 503);
  await assert.rejects(api("/users"), (error) => error.status === 503);
  assert.equal(calls, 2);
  calls = 0;
  globalThis.fetch = async (url, options) => { calls++; assert.equal(url, "/api/users"); assert.equal(options.method, "POST"); return reply({ id: "created" }); };
  assert.deepEqual(await api("/users", "POST", { username: "name" }), { id: "created" });
  assert.equal(calls, 1);
});
test("repeated cursors fail instead of looping", async () => {
  let calls = 0;
  globalThis.fetch = async () => { calls++; return reply([], "same"); };
  await assert.rejects(api("/groups"), (error) => error.status === 503);
  assert.equal(calls, 2);
});
