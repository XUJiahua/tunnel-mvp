"use client";
import { FormEvent, useCallback, useEffect, useRef, useState } from "react";
import { api } from "./api";
type Site = { id: string; name: string; legacyDisabled?: boolean };
type Agent = {
  id: string;
  siteID: string;
  name: string;
  disabled: boolean;
  requiresRotation: boolean;
  legacy: boolean;
  online: boolean;
  connectedAt?: string;
  activeChannels: number;
  remoteIP?: string;
  reportedVersion?: string;
};
export function AgentsPanel({
  sites,
  reload,
}: {
  sites: Site[];
  reload: () => Promise<void>;
}) {
  const [site, setSite] = useState(sites[0]?.id || ""),
    [agents, setAgents] = useState<Agent[]>([]),
    [name, setName] = useState(""),
    [error, setError] = useState(""),
    [busy, setBusy] = useState(false),
    [credential, setCredential] = useState<{
      instanceID: string;
      siteID: string;
      token: string;
    } | null>(null);
  const generation = useRef(0);
  const load = useCallback(async () => {
    const g = ++generation.current;
    if (!site) {
      setAgents([]);
      return;
    }
    try {
      const rows = await api<Agent[]>(`/sites/${site}/agents`);
      if (generation.current === g) setAgents(rows);
    } catch (e) {
      if (generation.current === g) setError(String(e));
    }
  }, [site]);
  useEffect(() => {
    setCredential(null);
    setError("");
    void load();
    const timer = setInterval(() => void load(), 5000);
    return () => {
      clearInterval(timer);
      generation.current++;
    };
  }, [load]);
  async function mutate(fn: () => Promise<unknown>) {
    setBusy(true);
    setError("");
    try {
      await fn();
      await load();
      await reload();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  async function create(e: FormEvent) {
    e.preventDefault();
    await mutate(async () => {
      const r = await api<{ agent: Agent; token: string }>(
        `/sites/${site}/agents`,
        "POST",
        { name },
      );
      setCredential({ instanceID: r.agent.id, siteID: site, token: r.token });
      setName("");
    });
  }
  const current = sites.find((s) => s.id === site);
  return (
    <section className="panel">
      <h2>Agent 实例</h2>
      <p>
        新连接分配到在线实例。已有连接固定使用原实例，故障后需要客户端重新连接。所有实例应能访问同一组应用。
      </p>
      {error && <p role="alert">{error}</p>}
      <label className="field">
        <span>Agent 所属站点</span>
        <select
          value={site}
          onChange={(e) => setSite(e.target.value)}
          disabled={busy}
        >
          {sites.map((s) => (
            <option key={s.id} value={s.id}>
              {s.name}
            </option>
          ))}
        </select>
      </label>
      {current && (
        <>
          <button
            disabled={busy}
            onClick={() =>
              void mutate(() =>
                api(`/sites/${site}`, "PATCH", {
                  legacyDisabled: !current.legacyDisabled,
                }),
              )
            }
          >
            {current.legacyDisabled
              ? "启用站点令牌兼容连接"
              : "禁用站点令牌兼容连接"}
          </button>
          <form onSubmit={create} className="inline-form">
            <label className="field">
              <span>实例名称</span>
              <input
                required
                maxLength={100}
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </label>
            <button className="primary-button" disabled={busy}>
              登记 Agent
            </button>
          </form>
        </>
      )}
      {credential && (
        <div role="status" className="notice">
          <strong>
            凭据只显示本次，请保存到 Agent 配置与受保护的环境文件。
          </strong>
          <pre>{JSON.stringify(credential, null, 2)}</pre>
          <p>配置 instanceID、siteID；使用 TUNNEL_AGENT_TOKEN 传入 token。</p>
          <button onClick={() => setCredential(null)}>已保存，隐藏凭据</button>
        </div>
      )}
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>实例</th>
              <th>状态</th>
              <th>连接</th>
              <th>版本 / 来源</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {agents.map((a) => (
              <tr key={a.id}>
                <td>
                  {a.name}
                  <small>{a.id}</small>
                </td>
                <td>
                  {a.requiresRotation
                    ? "必须轮换凭据"
                    : a.disabled
                      ? "已禁用"
                      : a.online
                        ? "在线"
                        : "离线"}
                </td>
                <td>
                  {a.activeChannels}
                  <small>
                    {a.connectedAt
                      ? new Date(a.connectedAt).toLocaleString()
                      : ""}
                  </small>
                </td>
                <td>
                  {a.reportedVersion || "—"}
                  <small>{a.remoteIP || "—"}</small>
                </td>
                <td>
                  {!a.legacy && (
                    <>
                      <button
                        disabled={busy || a.requiresRotation}
                        onClick={() =>
                          void mutate(() =>
                            api(`/sites/${site}/agents/${a.id}`, "PATCH", {
                              disabled: !a.disabled,
                            }),
                          )
                        }
                      >
                        {a.disabled ? "启用" : "禁用"}
                      </button>
                      <button
                        disabled={busy}
                        onClick={() =>
                          void mutate(async () => {
                            const r = await api<{
                              agent: Agent;
                              token: string;
                            }>(`/sites/${site}/agents/${a.id}/rotate`, "POST");
                            setCredential({
                              instanceID: a.id,
                              siteID: site,
                              token: r.token,
                            });
                          })
                        }
                      >
                        轮换凭据
                      </button>
                      <button
                        disabled={busy}
                        onClick={() => {
                          if (
                            window.confirm(`删除实例 ${a.name} 并关闭其连接？`)
                          )
                            void mutate(() =>
                              api(`/sites/${site}/agents/${a.id}`, "DELETE"),
                            );
                        }}
                      >
                        删除
                      </button>
                    </>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}
