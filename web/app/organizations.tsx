"use client";
import { FormEvent, useCallback, useEffect, useState } from "react";
import { api } from "./api";
type Org = { id: string; name: string };
type Role = {
  manageable?: boolean;
  id: string;
  name: string;
  organizationID?: string;
  permissions: string[];
};
type Member = {
  manageable?: boolean;
  id: string;
  username: string;
  role: string;
  organizationID?: string;
  roleID?: string;
};
const labels: Record<string, string> = {
  "roles:manage": "组织角色定义与分配（受自身能力上限约束）",
  "sites:manage": "站点、令牌与 Agent 管理",
  "applications:manage": "应用管理与诊断",
  "members:manage": "普通成员、密码与授权管理",
  "groups:manage": "用户组与邀请管理",
  "audit:read": "组织审计查询",
  "applications:access": "访问组织内全部应用",
};
export function OrganizationsPanel({
  onChanged,
  globalAdmin = true,
  organizationID = "default",
}: {
  onChanged: () => void;
  globalAdmin?: boolean;
  organizationID?: string;
}) {
  const [orgs, setOrgs] = useState<Org[]>([]),
    [roles, setRoles] = useState<Role[]>([]),
    [members, setMembers] = useState<Member[]>([]);
  const [permissions, setPermissions] = useState<string[]>([]),
    [editing, setEditing] = useState<Role | null>(null),
    [roleOrg, setRoleOrg] = useState(organizationID);
  const [error, setError] = useState(""),
    [busy, setBusy] = useState(false);
  const load = useCallback(async () => {
    try {
      const [o, r, m] = await Promise.all([
        api<Org[]>("/organizations"),
        api<{ roles: Role[]; permissions: string[] }>("/roles"),
        api<Member[]>(globalAdmin ? "/users" : "/role-members"),
      ]);
      setOrgs(o);
      setRoles(r.roles);
      setPermissions(r.permissions);
      setMembers(m);
    } catch (e) {
      setError((e as Error).message);
    }
  }, [globalAdmin]);
  useEffect(() => {
    void load();
  }, [load]);
  async function mutate(path: string, method: string, body?: unknown) {
    setBusy(true);
    setError("");
    try {
      await api(path, method, body);
      await load();
      onChanged();
      return true;
    } catch (e) {
      setError((e as Error).message);
      return false;
    } finally {
      setBusy(false);
    }
  }
  async function createOrg(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const f = e.currentTarget,
      d = new FormData(f);
    if (await mutate("/organizations", "POST", { name: d.get("name") }))
      f.reset();
  }
  async function saveRole(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const f = e.currentTarget,
      d = new FormData(f);
    if (
      await mutate(
        editing ? `/roles/${editing.id}` : "/roles",
        editing ? "PATCH" : "POST",
        {
          name: d.get("name"),
          organizationID: roleOrg,
          permissions: d.getAll("permissions"),
        },
      )
    ) {
      setEditing(null);
      f.reset();
    }
  }
  function orgName(id?: string) {
    return orgs.find((o) => o.id === (id || "default"))?.name || id;
  }
  return (
    <>
      <p className="permission-note">
        一个成员属于一个组织。组织角色只委派本组织管理能力；全局管理员可委派角色管理；组织操作员只能管理不超过自身能力的角色和成员。管理应用与访问应用分别授权。
      </p>
      {error && (
        <p className="form-error" role="alert">
          {error}
        </p>
      )}
      {globalAdmin && (
        <>
          <form className="group-form" onSubmit={createOrg}>
            <h3>创建组织</h3>
            <label className="field">
              <span>组织名称</span>
              <input name="name" required maxLength={100} />
            </label>
            <button className="primary" disabled={busy}>
              创建组织
            </button>
          </form>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>组织</th>
                  <th>ID</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {orgs.map((o) => (
                  <tr key={o.id}>
                    <td>{o.name}</td>
                    <td>
                      <code>{o.id}</code>
                    </td>
                    <td>
                      {o.id !== "default" && (
                        <>
                          <button
                            className="subtle"
                            disabled={busy}
                            onClick={() => {
                              const name = window.prompt(
                                "新的组织名称",
                                o.name,
                              );
                              if (name)
                                void mutate(`/organizations/${o.id}`, "PATCH", {
                                  name,
                                });
                            }}
                          >
                            重命名组织
                          </button>
                          <button
                            className="subtle danger"
                            disabled={busy}
                            onClick={() => {
                              if (
                                window.confirm(
                                  `删除空组织「${o.name}」？请先清理成员、站点、用户组、邀请和角色。`,
                                )
                              )
                                void mutate(`/organizations/${o.id}`, "DELETE");
                            }}
                          >
                            删除空组织
                          </button>
                        </>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
      <form
        key={editing?.id || "new"}
        className="group-form"
        onSubmit={saveRole}
      >
        <h3>{editing ? "编辑角色" : "创建角色"}</h3>
        <label className="field">
          <span>角色所属组织</span>
          <select
            aria-label="角色所属组织"
            value={roleOrg}
            disabled={!!editing || !globalAdmin}
            onChange={(e) => setRoleOrg(e.target.value)}
          >
            {orgs.map((o) => (
              <option key={o.id} value={o.id}>
                {o.name}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span>角色名称</span>
          <input
            name="name"
            defaultValue={editing?.name}
            required
            maxLength={100}
          />
        </label>
        <fieldset className="grants">
          <legend>组织能力</legend>
          {permissions.map((p) => (
            <label key={p}>
              <input
                type="checkbox"
                name="permissions"
                value={p}
                defaultChecked={editing?.permissions.includes(p)}
              />
              {labels[p] || p}
            </label>
          ))}
        </fieldset>
        <button className="primary" disabled={busy}>
          {editing ? "保存角色" : "创建角色"}
        </button>
        {editing && (
          <button
            type="button"
            className="secondary"
            onClick={() => setEditing(null)}
          >
            取消编辑
          </button>
        )}
      </form>
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>角色 / 组织</th>
              <th>能力</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {roles.map((r) => (
              <tr key={r.id}>
                <td>
                  <strong>{r.name}</strong>
                  <div>{orgName(r.organizationID)}</div>
                </td>
                <td>
                  {r.permissions.map((p) => labels[p] || p).join("、") ||
                    "无管理能力"}
                </td>
                <td>
                  <button
                    className="subtle"
                    disabled={busy || r.manageable === false}
                    onClick={() => {
                      setEditing(r);
                      setRoleOrg(r.organizationID || "default");
                    }}
                  >
                    编辑角色
                  </button>
                  <button
                    className="subtle danger"
                    disabled={busy || r.manageable === false}
                    onClick={() => {
                      if (
                        window.confirm(
                          `删除「${r.name}」并撤销所有持有者的会话？`,
                        )
                      )
                        void mutate(`/roles/${r.id}`, "DELETE");
                    }}
                  >
                    删除角色
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <h3>成员角色</h3>
      <p className="muted">
        角色变更会让该成员全部设备退出。只能分配自身权限范围内的角色；更高权限的成员由全局管理员维护。
      </p>
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>成员 / 组织</th>
              <th>组织角色</th>
            </tr>
          </thead>
          <tbody>
            {members
              .filter((m) => m.role !== "admin")
              .map((m) => (
                <tr key={m.id}>
                  <td>
                    {m.username}
                    <div>{orgName(m.organizationID)}</div>
                  </td>
                  <td>
                    <select
                      aria-label={`角色 · ${m.username}`}
                      disabled={busy || m.manageable === false}
                      value={m.roleID || ""}
                      onChange={(e) => {
                        if (
                          window.confirm(
                            `修改「${m.username}」角色并撤销旧设备？`,
                          )
                        )
                          void mutate(`/users/${m.id}/role`, "PATCH", {
                            roleID: e.target.value,
                          });
                      }}
                    >
                      <option value="">普通成员</option>
                      {roles
                        .filter(
                          (r) =>
                            (r.organizationID || "") ===
                            (m.organizationID || ""),
                        )
                        .map((r) => (
                          <option
                            disabled={r.manageable === false}
                            key={r.id}
                            value={r.id}
                          >
                            {r.name}
                          </option>
                        ))}
                    </select>
                  </td>
                </tr>
              ))}
          </tbody>
        </table>
      </div>
    </>
  );
}
