"use client";
import { FormEvent, useCallback, useEffect, useState } from "react";
import { api } from "./api";
import { Grants } from "./operations";
type Group = {
  id: string;
  name: string;
  memberIDs: string[];
  siteIDs: string[];
  serviceIDs: string[];
};
type Member = { id: string; username: string };
type Site = { id: string; name: string };
type Service = { id: string; siteID: string; name: string; url: string };
type Invite = {
  id: string;
  username: string;
  expiresAt: string;
  expired: boolean;
  groupIDs: string[];
};
export function GroupsPanel({
  members,
  sites,
  services,
}: {
  members: Member[];
  sites: Site[];
  services: Service[];
}) {
  const [groups, setGroups] = useState<Group[]>([]),
    [invites, setInvites] = useState<Invite[]>([]);
  const [editing, setEditing] = useState<Group | null | undefined>(undefined),
    [inviting, setInviting] = useState(false),
    [link, setLink] = useState("");
  const [error, setError] = useState(""),
    [busy, setBusy] = useState(false);
  const load = useCallback(async () => {
    try {
      const [g, i] = await Promise.all([
        api<Group[]>("/groups"),
        api<Invite[]>("/invitations"),
      ]);
      setGroups(g);
      setInvites(i);
    } catch (e) {
      setError((e as Error).message);
    }
  }, []);
  useEffect(() => {
    void load();
    const timer = setInterval(() => void load(), 10000);
    return () => clearInterval(timer);
  }, [load]);
  async function save(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const data = new FormData(e.currentTarget);
    setBusy(true);
    setError("");
    try {
      if (inviting) {
        const result = await api<{ url: string }>("/invitations", "POST", {
          username: data.get("username"),
          hours: Number(data.get("hours")),
          siteIDs: data.getAll("siteIDs"),
          serviceIDs: data.getAll("serviceIDs"),
          groupIDs: data.getAll("groupIDs"),
        });
        setLink(result.url);
        setInviting(false);
      } else {
        await api(
          editing ? `/groups/${editing.id}` : "/groups",
          editing ? "PATCH" : "POST",
          {
            name: data.get("name"),
            memberIDs: data.getAll("memberIDs"),
            siteIDs: data.getAll("siteIDs"),
            serviceIDs: data.getAll("serviceIDs"),
          },
        );
        setEditing(undefined);
      }
      await load();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  async function remove(kind: string, id: string) {
    if (
      !window.confirm(
        kind === "groups"
          ? "删除用户组会收回仅由该组授予的访问，成员账号保留。继续？"
          : "撤销此邀请？尚未使用的链接将立即失效。",
      )
    )
      return;
    try {
      await api(`/${kind}/${id}`, "DELETE");
      await load();
    } catch (e) {
      setError((e as Error).message);
    }
  }
  return (
    <>
      <div className="section-heading">
        <p className="muted">
          用户的直接授权与所有用户组授权取并集。组变更会立即关闭失去权限的连接。
        </p>
        <div className="row-actions">
          <button
            className="secondary"
            onClick={() => {
              setEditing(null);
              setInviting(false);
              setError("");
            }}
          >
            创建用户组
          </button>
          <button
            className="primary"
            onClick={() => {
              setInviting(true);
              setEditing(undefined);
              setError("");
            }}
          >
            邀请成员
          </button>
        </div>
      </div>
      {error && (
        <p role="alert" className="form-error">
          {error}
        </p>
      )}
      {link && (
        <div className="info-strip">
          <div>
            <strong>邀请链接（仅本次展示）</strong>
            <p className="invite-link">{link}</p>
            <p>请通过可信渠道交给成员。链接可使用一次；成员自行设置密码。</p>
            <button className="subtle" onClick={() => setLink("")}>
              隐藏链接
            </button>
          </div>
        </div>
      )}
      {(editing !== undefined || inviting) && (
        <form
          className="group-editor"
          onSubmit={save}
          key={inviting ? "invite" : editing?.id || "new"}
        >
          <h2>
            {inviting ? "邀请成员" : editing ? "编辑用户组" : "创建用户组"}
          </h2>
          {inviting ? (
            <>
              <label className="field">
                <span>受邀用户名</span>
                <input name="username" required maxLength={100} />
              </label>
              <label className="field">
                <span>有效小时</span>
                <input
                  name="hours"
                  type="number"
                  min={1}
                  max={168}
                  defaultValue={72}
                  required
                />
              </label>
              <fieldset className="grants">
                <legend>加入用户组</legend>
                {groups.map((g) => (
                  <label key={g.id}>
                    <input type="checkbox" name="groupIDs" value={g.id} />
                    {g.name}
                  </label>
                ))}
              </fieldset>
            </>
          ) : (
            <>
              <label className="field">
                <span>用户组名称</span>
                <input
                  name="name"
                  required
                  maxLength={100}
                  defaultValue={editing?.name || ""}
                />
              </label>
              <fieldset className="grants">
                <legend>组成员</legend>
                {members.map((m) => (
                  <label key={m.id}>
                    <input
                      type="checkbox"
                      name="memberIDs"
                      value={m.id}
                      defaultChecked={editing?.memberIDs?.includes(m.id)}
                    />
                    {m.username}
                  </label>
                ))}
              </fieldset>
            </>
          )}
          <Grants
            sites={sites}
            services={services}
            user={editing ? { ...editing, username: editing.name } : undefined}
          />
          <div className="modal-actions">
            <button
              type="button"
              className="secondary"
              onClick={() => {
                setEditing(undefined);
                setInviting(false);
              }}
            >
              取消
            </button>
            <button className="primary" disabled={busy}>
              {busy ? "保存中…" : "保存"}
            </button>
          </div>
        </form>
      )}
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>用户组</th>
              <th>成员</th>
              <th>整站 / 单独应用</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {groups.map((g) => (
              <tr key={g.id}>
                <td>
                  <strong>{g.name}</strong>
                </td>
                <td>
                  {g.memberIDs
                    ?.map(
                      (id) => members.find((m) => m.id === id)?.username || id,
                    )
                    .join("、") || "无成员"}
                </td>
                <td>
                  {g.siteIDs?.length || 0} / {g.serviceIDs?.length || 0}
                </td>
                <td>
                  <button
                    className="subtle"
                    onClick={() => {
                      setInviting(false);
                      setEditing(g);
                      setError("");
                    }}
                  >
                    编辑用户组
                  </button>
                  <button
                    className="subtle danger"
                    onClick={() => void remove("groups", g.id)}
                  >
                    删除用户组
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <h2>待使用邀请</h2>
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>用户名</th>
              <th>到期</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {invites.map((v) => (
              <tr key={v.id}>
                <td>{v.username}</td>
                <td>{new Date(v.expiresAt).toLocaleString()}</td>
                <td>{v.expired ? "已过期" : "待接受"}</td>
                <td>
                  <button
                    className="subtle danger"
                    onClick={() => void remove("invitations", v.id)}
                  >
                    撤销邀请
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  );
}
