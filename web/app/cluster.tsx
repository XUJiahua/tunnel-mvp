"use client";

import { useCallback, useEffect, useState } from "react";
import { api } from "./api";

type Node = {
  nodeID: string;
  nodeEpoch: string;
  alive: boolean;
  fresh: boolean;
  draining: boolean;
  reportedAt?: string;
  report?: {
    version: string;
    activeIngress: number;
    activeRelays: number;
    agents: number;
    agentChannels: number;
    failed: boolean;
    backlog: { pending: number; bytes: number; enqueueErrors: number };
  };
};
type Cluster = { enabled: boolean; localNodeID?: string; nodes?: Node[] };

export function ClusterPanel() {
  const [data, setData] = useState<Cluster | null>(null);
  const [error, setError] = useState("");
  const load = useCallback(async () => {
    try {
      setData(await api<Cluster>("/cluster/status"));
      setError("");
    } catch (e) {
      setError((e as Error).message);
    }
  }, []);
  useEffect(() => {
    void load();
    const timer = setInterval(() => void load(), 15000);
    return () => clearInterval(timer);
  }, [load]);
  if (data && !data.enabled && !error) return null;
  const nodes = data?.nodes || [];
  const complete = !error && nodes.length > 0 && nodes.every((n) => n.alive && n.fresh && !n.report?.failed);
  return (
    <section aria-label="集群节点" className="cluster-panel">
      <h2>集群节点</h2>
      <p className="muted small">
        当前响应节点：<code>{data?.localNodeID || "—"}</code>。每 15 秒刷新。
        {complete ? "全部节点正在上报。" : "观测覆盖不完整，请检查离线、旧版本或上报失败的节点。"}
      </p>
      {error && <p className="form-error" role="alert">{error}</p>}
      <div className="table-wrap">
        <table>
          <thead><tr><th>节点 / 版本</th><th>状态</th><th>入口 / 中继</th><th>Agent / 通道</th><th>待汇聚 / 写入错误</th><th>最近上报</th></tr></thead>
          <tbody>{nodes.map((node) => <tr key={node.nodeID + node.nodeEpoch}>
            <td><code>{node.nodeID}</code><br /><span className="small muted">{node.report?.version || "无版本报告"}</span></td>
            <td>{!node.alive ? "离线" : node.draining ? "排空中" : "在线"}{!node.fresh ? " · 未上报" : node.report?.failed ? " · 汇聚失败" : ""}</td>
            <td>{node.fresh ? `${node.report?.activeIngress ?? 0} / ${node.report?.activeRelays ?? 0}` : "—"}</td>
            <td>{node.fresh ? `${node.report?.agents ?? 0} / ${node.report?.agentChannels ?? 0}` : "—"}</td>
            <td>{node.fresh ? `${node.report?.backlog?.pending ?? 0} / ${node.report?.backlog?.enqueueErrors ?? 0}` : "—"}</td>
            <td>{node.reportedAt ? new Date(node.reportedAt).toLocaleString() : "—"}</td>
          </tr>)}</tbody>
        </table>
      </div>
    </section>
  );
}
