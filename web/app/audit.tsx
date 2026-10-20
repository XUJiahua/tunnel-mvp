"use client";
import { FormEvent, useEffect, useState } from "react";
import { api, APIError } from "./api";
type Entry = {
  eventID?: string;
  nodeID?: string;
  time: string;
  event: string;
  user: string;
  siteID: string;
  resourceID: string;
  diagnostic?: { code?: string; requestID?: string };
};
type Filters = {
  nodeID: string;
  event: string;
  siteID: string;
  user: string;
  resourceID: string;
  since: string;
};
type Page = {
  scope?: string;
  entries: Entry[];
  nextCursor?: string;
  indexedFiles?: number;
  skipped?: number;
};
const empty: Filters = {
  nodeID: "",
  event: "",
  siteID: "",
  user: "",
  resourceID: "",
  since: "",
};
export function AuditPanel() {
  const [inputs, setInputs] = useState<Filters>(empty),
    [applied, setApplied] = useState<Filters>(empty),
    [entries, setEntries] = useState<Entry[]>([]),
    [cursor, setCursor] = useState("");
  const [cluster, setCluster] = useState(false);
  const [files, setFiles] = useState(0),
    [skipped, setSkipped] = useState(0),
    [error, setError] = useState(""),
    [busy, setBusy] = useState(false);
  async function load(more = false) {
    setBusy(true);
    setError("");
    const filters = more
      ? applied
      : {
          ...inputs,
          since: inputs.since ? new Date(inputs.since).toISOString() : "",
        };
    try {
      const query = new URLSearchParams({
        ...filters,
        limit: "100",
        ...(more ? { cursor } : {}),
      });
      const result = await api<Page>(`/audit?${query}`);
      setEntries((previous) =>
        more ? [...previous, ...result.entries] : result.entries,
      );
      setCursor(result.nextCursor || "");
      setCluster(result.scope === "cluster");
      setFiles(result.indexedFiles || 0);
      setSkipped(result.skipped || 0);
      if (!more) setApplied(filters);
    } catch (e) {
      setError((e as Error).message);
      if (e instanceof APIError && e.status === 409) setCursor("");
    } finally {
      setBusy(false);
    }
  }
  useEffect(() => {
    void load();
  }, []);
  function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    void load();
  }
  function input(key: keyof Filters, label: string, type = "text") {
    return (
      <label className="field">
        <span>{label}</span>
        <input
          type={type}
          value={inputs[key]}
          onChange={(e) => setInputs({ ...inputs, [key]: e.target.value })}
        />
      </label>
    );
  }
  return (
    <>
      <form className="audit-filters" onSubmit={submit}>
        {cluster && input("nodeID", "来源节点")}
        {input("event", "事件类型")}
        {input("siteID", "站点 ID")}
        {input("user", "操作者")}
        {input("resourceID", "资源 ID")}
        {input("since", "起始时间", "datetime-local")}
        <button className="secondary" disabled={busy}>
          {busy ? "查询中…" : "查询审计"}
        </button>
      </form>
      <p className="muted small">
        {cluster ? "查询已汇聚的集群审计，按接收顺序倒序。待汇聚记录稍后可见。" : "查询当前和全部保留轮转文件，按记录追加顺序倒序。"}
        每页 100 条，可加载更早记录。{files ? `已索引 ${files} 个文件。` : ""}
      </p>
      {skipped > 0 && (
        <p className="form-error" role="alert">
          跳过 {skipped} 条无效或过大记录，原始文件保留。
        </p>
      )}
      {error && (
        <p className="form-error" role="alert">
          {error}
        </p>
      )}
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>时间</th>
              <th>事件</th>
              {cluster && <th>来源节点</th>}
              <th>操作者</th>
              <th>站点 / 资源</th>
              <th>诊断</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((entry, i) => (
              <tr key={entry.eventID || i}>
                <td>
                  {entry.time
                    ? new Date(entry.time).toLocaleString()
                    : "未知时间"}
                </td>
                <td>
                  <code>{entry.event}</code>
                </td>
                {cluster && <td><code>{entry.nodeID || "未知来源"}</code></td>}
                <td>{entry.user || "Agent"}</td>
                <td>
                  <code>{entry.siteID || "—"}</code>
                  <br />
                  <code>{entry.resourceID || "—"}</code>
                </td>
                <td>
                  <code>{entry.diagnostic?.code || "—"}</code>
                  {entry.diagnostic?.requestID && (
                    <div className="small muted">
                      {entry.diagnostic.requestID}
                    </div>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {!entries.length && <p className="muted">没有匹配的审计记录。</p>}
      {cursor && (
        <button
          className="secondary"
          disabled={busy}
          onClick={() => void load(true)}
        >
          加载更早记录
        </button>
      )}
    </>
  );
}
