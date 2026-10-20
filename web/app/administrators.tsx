"use client";
import { FormEvent, useState } from "react";
import { api } from "./api";
type Member = {
  id: string;
  username: string;
  role: string;
  organizationID?: string;
  disabled?: boolean;
  passwordLogin?: boolean;
};
export function AdministratorsPanel({
  members,
  onChanged,
  onExpired,
}: {
  members: Member[];
  onChanged: () => void;
  onExpired: () => void;
}) {
  const [secret, setSecret] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  async function mutate(
    path: string,
    method: string,
    body: Record<string, string>,
  ) {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const result = await api<{ reauthenticate: boolean }>(path, method, {
        ...body,
        currentPassword: secret,
      });
      setSecret("");
      if (result.reauthenticate) onExpired();
      else {
        onChanged();
        setNotice("管理员变更已保存，目标账号的旧会话已失效。");
      }
    } catch (e) {
      setError((e as Error).message);
      setSecret("");
    } finally {
      setBusy(false);
    }
  }
  async function create(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const form = e.currentTarget;
    const data = new FormData(form);
    await mutate("/administrators", "POST", {
      username: String(data.get("username")),
      password: String(data.get("password")),
    });
    form.reset();
  }
  function action(member: Member, action: string) {
    if (
      !window.confirm(
        `确定对「${member.username}」执行${({ promote: "提升为全局管理员", demote: "降为成员", disable: "停用", enable: "启用", delete: "删除" } as Record<string, string>)[action]}？该账号全部旧会话会立即退出。`,
      )
    )
      return;
    void mutate(
      `/administrators/${member.id}`,
      action === "delete" ? "DELETE" : "PATCH",
      action === "delete" ? {} : { action },
    );
  }
  return (
    <>
      <p className="permission-note">
        全局管理员拥有全部站点和成员的管理与访问权限。系统始终保留至少一个启用的本地密码管理员；请为每位管理员创建独立账号。
      </p>
      <label className="field">
        <span>当前管理员密码</span>
        <input
          type="password"
          autoComplete="current-password"
          value={secret}
          onChange={(e) => setSecret(e.target.value)}
        />
      </label>
      {error && (
        <p className="form-error" role="alert">
          {error}
        </p>
      )}
      {notice && <p role="status">{notice}</p>}
      <form onSubmit={create} className="group-form">
        <h3>新增管理员</h3>
        <label className="field">
          <span>新管理员用户名</span>
          <input name="username" required maxLength={100} autoComplete="off" />
        </label>
        <label className="field">
          <span>新管理员初始密码</span>
          <input
            type="password"
            name="password"
            required
            minLength={12}
            maxLength={72}
            autoComplete="new-password"
          />
        </label>
        <button className="primary" disabled={busy || !secret}>
          创建管理员
        </button>
      </form>
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>账号</th>
              <th>身份与状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {members.map((m) => (
              <tr key={m.id}>
                <td>{m.username}</td>
                <td>
                  {m.role === "admin" ? "全局管理员" : "成员"}
                  {m.disabled ? " · 已停用" : ""}
                </td>
                <td>
                  {m.role === "admin" ? (
                    <>
                      <button
                        className="subtle"
                        disabled={busy || !secret}
                        onClick={() =>
                          action(m, m.disabled ? "enable" : "disable")
                        }
                      >
                        {m.disabled ? "启用管理员" : "停用管理员"}
                      </button>
                      <button
                        className="subtle danger"
                        disabled={busy || !secret}
                        onClick={() => action(m, "demote")}
                      >
                        降为成员
                      </button>
                      <button
                        className="subtle danger"
                        disabled={busy || !secret}
                        onClick={() => action(m, "delete")}
                      >
                        删除管理员
                      </button>
                    </>
                  ) : (
                    !m.organizationID &&
                    m.passwordLogin &&
                    !m.disabled && (
                      <button
                        className="subtle"
                        disabled={busy || !secret}
                        onClick={() => action(m, "promote")}
                      >
                        提升为管理员
                      </button>
                    )
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  );
}
