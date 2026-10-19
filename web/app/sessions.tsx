"use client";
import { useCallback, useEffect, useState } from "react";
import { api, APIError } from "./api";
type Session = {
  id: string;
  userID: string;
  username: string;
  method: string;
  createdAt: string;
  lastSeen: string;
  expires: string;
  userAgent: string;
  remoteIP: string;
  current: boolean;
};
export function SessionsPanel({
  admin,
  onExpired,
}: {
  admin: boolean;
  onExpired: () => void;
}) {
  const [all, setAll] = useState(false);
  const [sessions, setSessions] = useState<Session[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const endpoint = all ? "/sessions" : "/me/sessions";
  const load = useCallback(async () => {
    try {
      setSessions(await api<Session[]>(endpoint));
      setError("");
    } catch (e) {
      if (e instanceof APIError && e.status === 401) onExpired();
      else setError((e as Error).message);
    }
  }, [endpoint, onExpired]);
  useEffect(() => {
    void load();
  }, [load]);
  async function revoke(session: Session) {
    if (
      !window.confirm(
        `撤销「${session.username}」的${session.current ? "当前" : "此设备"}会话？该会话的应用连接将立即关闭。`,
      )
    )
      return;
    setBusy(true);
    try {
      await api(`${endpoint}/${session.id}`, "DELETE");
      if (session.current) onExpired();
      else await load();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      <div className="section-heading">
        <p className="muted">
          会话仅保留在当前 Server；IP
          为直连地址，不信任转发头。撤销会同时关闭该会话的应用连接。
        </p>
        <button className="secondary" onClick={() => void load()}>
          刷新会话
        </button>
      </div>
      {admin && (
        <label className="field">
          <span>会话范围</span>
          <select
            value={all ? "all" : "mine"}
            onChange={(e) => setAll(e.target.value === "all")}
          >
            <option value="mine">我的会话</option>
            <option value="all">全部成员会话</option>
          </select>
        </label>
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
              <th>账号 / 登录方式</th>
              <th>设备与地址</th>
              <th>创建 / 最近活动</th>
              <th>到期时间</th>
              <th className="right">操作</th>
            </tr>
          </thead>
          <tbody>
            {sessions.map((session) => (
              <tr key={session.id}>
                <td>
                  <strong>{session.username}</strong>
                  <div className="muted small">
                    {session.method === "oidc" ? "OIDC" : "本地密码"}
                    {session.current ? " · 当前会话" : ""}
                  </div>
                </td>
                <td>
                  <div className="session-agent" title={session.userAgent}>
                    {session.userAgent || "未知设备"}
                  </div>
                  <code>{session.remoteIP}</code>
                </td>
                <td>
                  {new Date(session.createdAt).toLocaleString()}
                  <div className="muted small">
                    {new Date(session.lastSeen).toLocaleString()}
                  </div>
                </td>
                <td>{new Date(session.expires).toLocaleString()}</td>
                <td className="right">
                  <button
                    className="subtle danger"
                    disabled={busy}
                    onClick={() => void revoke(session)}
                  >
                    撤销会话
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {!sessions.length && <p className="muted">暂无活跃会话。</p>}
    </>
  );
}
