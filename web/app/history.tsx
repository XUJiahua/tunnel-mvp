"use client";
import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "./api";
type Service = { id: string; name: string; url: string };
type Sample = {
  nodeID?: string;
  checkedAt: string;
  reachable: boolean;
  healthy: boolean;
  status: number;
  latencyMs: number;
  code?: string;
};
type History = {
  samples: Sample[];
  retentionHours: number;
  maxSamples: number;
  probeIntervalSeconds: number;
};
export function MonitoringHistory({ services }: { services: Service[] }) {
  const generation = useRef(0);
  const [id, setID] = useState(services[0]?.id || ""),
    [hours, setHours] = useState("24"),
    [data, setData] = useState<History | null>(null),
    [error, setError] = useState(""),
    [busy, setBusy] = useState(false);
  const selected = services.find((s) => s.id === id) || services[0],
    serviceID = selected?.id || "";
  const load = useCallback(async () => {
    const request = ++generation.current;
    if (!serviceID) {
      setData(null);
      return;
    }
    try {
      const since = new Date(
        Date.now() - Number(hours) * 3600000,
      ).toISOString();
      const result = await api<History>(
        `/services/${serviceID}/history?since=${encodeURIComponent(since)}`,
      );
      if (request === generation.current) {
        setData(result);
        setError("");
      }
    } catch (e) {
      if (request === generation.current) setError((e as Error).message);
    }
  }, [serviceID, hours]);
  useEffect(() => {
    setData(null);
    void load();
    const timer = setInterval(() => void load(), 15000);
    return () => {
      clearInterval(timer);
      generation.current++;
    };
  }, [load]);
  async function probe() {
    setBusy(true);
    try {
      await api(`/services/${serviceID}/probe`, "POST");
      await load();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  const samples = data?.samples || [],
    max = Math.max(1, ...samples.map((s) => s.latencyMs)),
    healthy = samples.filter((s) => s.healthy).length,
    points = samples
      .map(
        (s, i) =>
          `${40 + (i * 660) / Math.max(1, samples.length - 1)},${140 - (s.latencyMs / max) * 110}`,
      )
      .join(" ");
  return (
    <>
      <div className="history-controls">
        <label className="field">
          <span>监控应用</span>
          <select
            aria-label="监控应用"
            value={serviceID}
            onChange={(e) => setID(e.target.value)}
          >
            {services.map((s) => (
              <option key={s.id} value={s.id}>
                {s.name}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span>历史窗口</span>
          <select
            aria-label="历史窗口"
            value={hours}
            onChange={(e) => setHours(e.target.value)}
          >
            <option value="1">最近 1 小时</option>
            <option value="24">最近 24 小时</option>
            <option value="168">最近 7 天</option>
          </select>
        </label>
        <button
          className="secondary"
          disabled={!serviceID || busy}
          onClick={() => void probe()}
        >
          检测并刷新
        </button>
      </div>
      {error && (
        <p className="form-error" role="alert">
          {error}
        </p>
      )}
      {selected && (
        <p className="muted">
          {selected.name} · <code>{selected.url}</code>
        </p>
      )}
      {!samples.length ? (
        <p className="muted">当前检查配置暂无历史，点击“检测并刷新”采集。</p>
      ) : (
        <>
          <p role="status">
            {samples.length} 次检查 · {healthy} 次健康 ·{" "}
            {Math.round((healthy / samples.length) * 100)}% 健康比例
          </p>
          <svg
            className="history-chart"
            viewBox="0 0 720 190"
            role="img"
            aria-label="应用健康状态与响应延迟历史"
          >
            <line
              x1="40"
              y1="140"
              x2="700"
              y2="140"
              stroke="currentColor"
              opacity=".3"
            />
            <text x="2" y="28" fontSize="12" fill="currentColor">
              {max} ms
            </text>
            <text x="8" y="142" fontSize="12" fill="currentColor">
              0
            </text>
            <polyline
              points={points}
              fill="none"
              stroke="var(--accent, #378260)"
              strokeWidth="2"
            />
            {samples.map((s, i) => (
              <g key={`${s.checkedAt}-${i}`}>
                <circle
                  cx={40 + (i * 660) / Math.max(1, samples.length - 1)}
                  cy={140 - (s.latencyMs / max) * 110}
                  r="2"
                  fill="var(--accent, #378260)"
                />
                <rect
                  x={40 + (i * 660) / Math.max(1, samples.length)}
                  y="155"
                  width={Math.max(1, 660 / samples.length)}
                  height="12"
                  fill={
                    s.healthy ? "#378260" : s.reachable ? "#c38c2f" : "#be4b47"
                  }
                >
                  <title>
                    {new Date(s.checkedAt).toLocaleString()} ·{" "}
                    {s.status || s.code || "不可达"} · {s.latencyMs} ms{s.nodeID ? ` · ${s.nodeID}` : ""}
                  </title>
                </rect>
              </g>
            ))}
          </svg>
          <p className="muted small">
            绿：符合健康策略；黄：HTTP 可达但状态不符；红：连接失败。保留期{" "}
            {data?.retentionHours} 小时，每应用最多 {data?.maxSamples}{" "}
            条，采集间隔影响实际覆盖时间；修改检查配置后显示对应配置的历史。
          </p>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>检查时间</th>
                  {samples.some((s) => s.nodeID) && <th>来源节点</th>}
                  <th>健康状态</th>
                  <th>HTTP / 错误</th>
                  <th>延迟</th>
                </tr>
              </thead>
              <tbody>
                {samples
                  .slice(-30)
                  .reverse()
                  .map((s, i) => (
                    <tr key={`${s.checkedAt}-${i}`}>
                      <td>{new Date(s.checkedAt).toLocaleString()}</td>
                      {samples.some((sample) => sample.nodeID) && <td><code>{s.nodeID || "未知来源"}</code></td>}
                      <td>
                        {s.healthy
                          ? "健康"
                          : s.reachable
                            ? "状态不符"
                            : "不可达"}
                      </td>
                      <td>{s.status || s.code || "—"}</td>
                      <td>{s.latencyMs} ms</td>
                    </tr>
                  ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </>
  );
}
