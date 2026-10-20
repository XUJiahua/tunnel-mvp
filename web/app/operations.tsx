"use client";

import { FormEvent, useEffect, useRef, useState } from "react";
import { api } from "./api";

type Site = { id: string; name: string };
type Service = {
  customDomain?: string;
  healthPath?: string;
  connectionMode?: string;
  healthMethod?: string;
  healthExpectedStatus?: number;
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
          connectionMode: data.get("connectionMode"),
          healthPath: data.get("healthPath"),
          healthMethod: data.get("healthMethod"),
          healthExpectedStatus: Number(data.get("healthExpectedStatus")),
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
            <fieldset className="grants">
              <legend>业务健康检查</legend>
              <label className="field">
                <span>上游连接模式</span>
                <select
                  aria-label="上游连接模式"
                  name="connectionMode"
                  defaultValue={editor.service.connectionMode || ""}
                >
                  <option value="">按应用共享连接池</option>
                  <option value="session">每个登录会话独立串行连接</option>
                </select>
                <small>
                  连接缓存身份的应用可选独立模式。长连接会占用该会话的通道，其他请求需要等待；实际
                  NTLM/Kerberos 仍需目标平台验收。
                </small>
              </label>
              <label className="field">
                <span>健康检查相对路径</span>
                <input
                  name="healthPath"
                  defaultValue={editor.service.healthPath || ""}
                  placeholder="ready 或 health/status"
                  maxLength={256}
                />
              </label>
              <label className="field">
                <span>健康检查方法</span>
                <select
                  aria-label="健康检查方法"
                  name="healthMethod"
                  defaultValue={editor.service.healthMethod || "HEAD"}
                >
                  <option value="HEAD">HEAD</option>
                  <option value="GET">GET</option>
                </select>
              </label>
              <label className="field">
                <span>健康预期状态</span>
                <input
                  type="number"
                  name="healthExpectedStatus"
                  min={0}
                  max={599}
                  defaultValue={editor.service.healthExpectedStatus || 0}
                />
              </label>
              <p className="form-tip">
                路径相对于应用挂载前缀；留空检查应用入口。状态 0 接受
                200–399，或指定 200–599 的单个状态。检查不跟随重定向，不携带用户
                Cookie。
              </p>
            </fieldset>
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

export { AuditPanel } from "./audit";
