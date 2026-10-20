export class APIError extends Error {
  constructor(message: string, public status: number) { super(message); }
}

const directories = new Set([
  "/sites", "/services", "/users", "/groups", "/roles", "/organizations",
  "/group-members", "/role-members", "/invitations",
]);

async function request(path: string, method: string, body?: unknown) {
  const res = await fetch(`/api${path}`, {
    method, credentials: "same-origin", cache: "no-store",
    headers: body === undefined ? {} : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await res.json();
  if (!res.ok) throw new APIError(data.error || `请求失败 (${res.status})`, res.status);
  return { data, cursor: res.headers.get("X-Tunnel-Next-Cursor") };
}

// Keep existing panel data shapes while bounding each directory response. A
// revision conflict discards the entire partial collection before one retry.
export async function api<T>(path: string, method = "GET", body?: unknown): Promise<T> {
  if (method !== "GET" || !directories.has(path))
    return (await request(path, method, body)).data as T;
  for (let attempt = 0; ; attempt++) {
    try {
      let cursor: string | null = null;
      let items: unknown[] = [];
      let metadata: Record<string, unknown> = {};
      const seen = new Set<string>();
      do {
        const params = new URLSearchParams({ limit: "100" });
        if (cursor) params.set("cursor", cursor);
        const page = await request(`${path}?${params}`, method);
        if (path === "/roles") {
          items = items.concat(page.data.roles);
          metadata = page.data;
        } else items = items.concat(page.data);
        cursor = page.cursor;
        if (cursor && seen.has(cursor)) throw new APIError("分页未能前进，请刷新重试", 503);
        if (cursor) seen.add(cursor);
      } while (cursor);
      return (path === "/roles" ? { ...metadata, roles: items } : items) as T;
    } catch (error) {
      if (attempt === 0 && error instanceof APIError && error.status === 409) continue;
      throw error;
    }
  }
}
