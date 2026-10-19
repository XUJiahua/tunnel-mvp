"use client";

import { FormEvent, useEffect, useRef, useState } from "react";
import { api } from "./api";

type Site = { id: string; name: string };
type Service = {
  customDomain?: string;
  id: string;
  siteID: string;
  name: string;
  url: string;
};
type User = {
  id: string;
  organizationID?: string;
  username: string;
  siteIDs: string[];
  serviceIDs?: string[];
  identity?: { issuer: string; subject: string };
  passwordLogin?: boolean;
};
export type Editor =
  | { kind: "site"; site: Site }
  | { kind: "service"; service: Service }
  | { kind: "grants"; user: User }
  | { kind: "reset"; user: User }
  | { kind: "identity"; user: User }
  | { kind: "password" };

export function Grants({
  sites,
  services,
  user,
}: {
  sites: Site[];
  services: Service[];
  user?: User;
}) {
  return (
    <>
      <fieldset className="grants">
        <legend>整站授权（包含今后新增的应用）</legend>
        {sites.map((site) => (
          <label key={site.id}>
            <input
              type="checkbox"
              name="siteIDs"
              value={site.id}
              defaultChecked={user?.siteIDs?.includes(site.id)}
            />
            {site.name}
          </label>
        ))}
        {!sites.length && <p className="muted">尚无站点。</p>}
      </fieldset>
      <fieldset className="grants">
        <legend>单独应用授权</legend>
        {services.map((service) => (
          <label key={service.id}>
            <input
              type="checkbox"
              name="serviceIDs"
              value={service.id}
              defaultChecked={user?.serviceIDs?.includes(service.id)}
            />
            {service.name} ·{" "}
            {sites.find((site) => site.id === service.siteID)?.name}
          </label>
        ))}
        {!services.length && <p className="muted">尚无应用。</p>}
      </fieldset>
      <p className="form-tip">
        两类权限取并集。只授权个别应用时，请取消该站点的整站授权。
      </p>
    </>
  );
}

export function ResourceEditor({
  editor,
  oidcIssuer,
  globalAdmin,
  sites,
  services,
  onClose,
  onSaved,
}: {
  editor: Editor;
  oidcIssuer: string;
  globalAdmin: boolean;
  sites: Site[];
  services: Service[];
  onClose: () => void;
  onSaved: (reauthenticate: boolean) => Promise<void>;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    ref.current?.showModal();
  }, []);
  const title =
    editor.kind === "site"
      ? "编辑站点"
      : editor.kind === "service"
        ? "编辑应用"
        : editor.kind === "grants"
          ? `编辑权限 · ${editor.user.username}`
          : editor.kind === "identity"
            ? `身份绑定 · ${editor.user.username}`
            : editor.kind === "reset"
              ? `重置密码 · ${editor.user.username}`
              : "修改我的密码";
  async function save(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    setBusy(true);
    setError("");
    const data = new FormData(e.currentTarget);
    try {
      if (editor.kind === "site")
        await api(`/sites/${editor.site.id}`, "PATCH", {
          name: data.get("name"),
        });
      if (editor.kind === "service")
        await api(`/services/${editor.service.id}`, "PATCH", {
          name: data.get("name"),
          url: data.get("url"),
          siteID: data.get("siteID"),
          ...(globalAdmin ? { customDomain: data.get("customDomain") } : {}),
        });
      if (editor.kind === "grants")
        await api(`/users/${editor.user.id}`, "PATCH", {
          siteIDs: data.getAll("siteIDs"),
          serviceIDs: data.getAll("serviceIDs"),
        });
      if (editor.kind === "identity")
        await api(`/users/${editor.user.id}/identity`, "PATCH", {
          subject: data.get("subject"),
        });
      if (editor.kind === "reset")
        await api(`/users/${editor.user.id}/password`, "POST", {
          password: data.get("password"),
        });
      if (editor.kind === "password") {
        if (data.get("password") !== data.get("confirmPassword"))
          throw new Error("两次输入的新密码不一致。");
        await api("/me/password", "POST", {
          currentPassword: data.get("currentPassword"),
          password: data.get("password"),
        });
      }
      await onSaved(editor.kind === "password");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <dialog ref={ref} className="modal" onCancel={onClose}>
      <div className="modal-head">
        <h2>{title}</h2>
        <button className="icon-button" aria-label="关闭" onClick={onClose}>
          ×
        </button>
      </div>
      <form onSubmit={save}>
        {(editor.kind === "site" || editor.kind === "service") && (
          <label className="field">
            <span>{editor.kind === "site" ? "站点名称" : "应用名称"}</span>
            <input
              autoFocus
              name="name"
              required
              maxLength={100}
              defaultValue={
                editor.kind === "site" ? editor.site.name : editor.service.name
              }
            />
          </label>
        )}
        {editor.kind === "service" && (
          <>
            <label className="field">
              <span>内网地址</span>
              <input
                name="url"
                type="url"
                required
                defaultValue={editor.service.url}
              />
            </label>
            <label className="field">
              <span>迁移目标站点</span>
              <select
                aria-label="迁移目标站点"
                name="siteID"
                defaultValue={editor.service.siteID}
              >
                {sites.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name}
                  </option>
                ))}
              </select>
            </label>
            {globalAdmin && (
              <label className="field">
                <span>自定义访问域名</span>
                <input
                  name="customDomain"
                  defaultValue={editor.service.customDomain || ""}
                  placeholder="reports.example.net"
                />
              </label>
            )}
            <p className="form-tip">
              修改目标、站点或访问域名会关闭现有连接并撤销访问票据。路径目标在公网保留相同前缀，应用自身需配置子路径；自定义域名需另行配置
              DNS 和 TLS。应用 ID
              和单应用授权保留，整站授权按新站点计算。换成无关应用请新建应用，避免复用上游
              Cookie。
            </p>
          </>
        )}
        {editor.kind === "grants" && (
          <>
            <Grants sites={sites} services={services} user={editor.user} />
            <p className="form-tip">
              收回权限立即关闭受影响的 WebSocket、SSE 和其他代理请求。
            </p>
          </>
        )}
        {editor.kind === "identity" && (
          <>
            <p className="form-tip">
              修改绑定会立即撤销该成员所有会话和应用连接。邮箱或显示名不会参与身份匹配。
            </p>
            <p className="muted small">
              当前 Issuer：{editor.user.identity?.issuer || "未绑定"}
              <br />
              目标 Issuer：{oidcIssuer || "未配置"}
            </p>
            <label className="field">
              <span>OIDC Subject</span>
              <input
                name="subject"
                defaultValue={editor.user.identity?.subject || ""}
                maxLength={255}
                required={!editor.user.passwordLogin}
                autoComplete="off"
              />
            </label>
            <p className="form-tip">
              本地密码成员可清空 Subject 解除绑定；OIDC
              专用成员请通过禁用或删除收回访问。
            </p>
          </>
        )}
        {editor.kind === "password" && (
          <label className="field">
            <span>当前密码</span>
            <input
              autoFocus
              name="currentPassword"
              type="password"
              autoComplete="current-password"
              required
            />
          </label>
        )}
        {(editor.kind === "password" || editor.kind === "reset") && (
          <>
            <label className="field">
              <span>新密码</span>
              <input
                name="password"
                type="password"
                autoComplete="new-password"
                minLength={12}
                maxLength={72}
                required
              />
            </label>
            {editor.kind === "password" && (
              <label className="field">
                <span>确认新密码</span>
                <input
                  name="confirmPassword"
                  type="password"
                  autoComplete="new-password"
                  minLength={12}
                  maxLength={72}
                  required
                />
              </label>
            )}
            <p className="form-tip">
              密码须为 12–72
              字节。保存后该账号的全部登录会话失效，现有应用连接关闭，需要重新登录。
            </p>
          </>
        )}
        {error && (
          <p className="form-error" role="alert">
            {error}
          </p>
        )}
        <div className="modal-actions">
          <button
            type="button"
            className="secondary"
            onClick={onClose}
            disabled={busy}
          >
            取消
          </button>
          <button className="primary" disabled={busy}>
            {busy ? "保存中…" : "保存"}
          </button>
        </div>
      </form>
    </dialog>
  );
}

type AuditEntry = {
  time: string;
  event: string;
  user: string;
  siteID: string;
  resourceID: string;
  diagnostic?: { code?: string; requestID?: string };
};
export function AuditPanel() {
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  const [event, setEvent] = useState("");
  const [siteID, setSiteID] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  async function load() {
    setBusy(true);
    setError("");
    try {
      const data = await api<{ entries: AuditEntry[] }>(
        `/audit?${new URLSearchParams({ event, siteID, limit: "100" })}`,
      );
      setEntries(data.entries);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  useEffect(() => {
    void load();
  }, []); // Filters apply on explicit submission.
  return (
    <>
      <form
        className="audit-filters"
        onSubmit={(e) => {
          e.preventDefault();
          void load();
        }}
      >
        <label className="field">
          <span>事件类型</span>
          <input
            placeholder="例如 health_changed"
            value={event}
            onChange={(e) => setEvent(e.target.value)}
          />
        </label>
        <label className="field">
          <span>站点 ID</span>
          <input value={siteID} onChange={(e) => setSiteID(e.target.value)} />
        </label>
        <button className="secondary" disabled={busy}>
          {busy ? "查询中…" : "查询审计"}
        </button>
      </form>
      <p className="muted small">
        展示当前审计文件最后 2 MiB 中匹配的最近 100
        条记录，按时间倒序；历史轮转文件请使用日志采集系统查询。
      </p>
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
              <th>操作者</th>
              <th>站点 / 资源</th>
              <th>诊断</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((entry, i) => (
              <tr key={i}>
                <td>{new Date(entry.time).toLocaleString()}</td>
                <td>
                  <code>{entry.event}</code>
                </td>
                <td>{entry.user || "Agent"}</td>
                <td>
                  <code>
                    {entry.siteID || "—"}
                    <br />
                    {entry.resourceID}
                  </code>
                </td>
                <td>
                  <code>
                    {entry.diagnostic?.code || "—"}
                    <br />
                    {entry.diagnostic?.requestID}
                  </code>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {!entries.length && <p className="muted">暂无匹配记录。</p>}
    </>
  );
}
