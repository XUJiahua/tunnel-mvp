"use client";

import {
  FormEvent,
  ReactNode,
  useCallback,
  useEffect,
  useRef,
  useState,
} from "react";

import { BulkApplications } from "./bulk";
import { OrganizationsPanel } from "./organizations";
import { AdministratorsPanel } from "./administrators";
import { GroupsPanel } from "./groups";
import { InvitationLanding } from "./invitation";
import { SessionsPanel } from "./sessions";
import { api, APIError } from "./api";
import { AuditPanel, Editor, Grants, ResourceEditor } from "./operations";

type User = {
  id: string;
  username: string;
  role: "admin" | "viewer";
  organizationID?: string;
  roleID?: string;
  permissions?: string[];
  siteIDs: string[];
  serviceIDs?: string[];
  disabled?: boolean;
  identity?: { issuer: string; subject: string };
  passwordLogin?: boolean;
  sessionMethod?: string;
};
type Site = {
  organizationID?: string;
  id: string;
  name: string;
  online: boolean;
  createdAt: string;
};
type Service = {
  organizationID?: string;
  accessible?: boolean;
  customDomain?: string;
  id: string;
  name: string;
  siteID: string;
  url: string;
};
type Probe = {
  reachable: boolean;
  healthy?: boolean;
  serviceID?: string;
  status?: number;
  latencyMs?: number;
  message?: string;
  code?: string;
  stage?: string;
  hint?: string;
  requestID?: string;
  agentRequestID?: string;
  siteID?: string;
  target?: string;
  checkedAt?: string;
  resolvedIPs?: string[];
  attempts?: { ip: string; allowed: boolean; error?: string }[];
};
type Dialog = "site" | "service" | "user" | null;
type Enrollment = { id: string; name: string; token: string };

function Icon({ name, size = 20 }: { name: string; size?: number }) {
  const paths: Record<string, ReactNode> = {
    grid: (
      <>
        <rect x="3" y="3" width="7" height="7" rx="1.5" />
        <rect x="14" y="3" width="7" height="7" rx="1.5" />
        <rect x="3" y="14" width="7" height="7" rx="1.5" />
        <rect x="14" y="14" width="7" height="7" rx="1.5" />
      </>
    ),
    server: (
      <>
        <rect x="3" y="3" width="18" height="7" rx="2" />
        <rect x="3" y="14" width="18" height="7" rx="2" />
        <path d="M7 6.5h.01M7 17.5h.01M15 6.5h3M15 17.5h3" />
      </>
    ),
    apps: (
      <>
        <rect x="3" y="4" width="18" height="16" rx="2" />
        <path d="M3 9h18M7 6.5h.01M10 6.5h.01M8 13h8M8 16h5" />
      </>
    ),
    users: (
      <>
        <circle cx="9" cy="8" r="3" />
        <path d="M3 21v-3a6 6 0 0 1 12 0v3M16 5a3 3 0 0 1 0 6M21 21v-3a6 6 0 0 0-4-5" />
      </>
    ),
    arrow: (
      <>
        <path d="M7 17 17 7M7 7h10v10" />
      </>
    ),
    plus: <path d="M12 5v14M5 12h14" />,
    refresh: (
      <>
        <path d="M20 7v5h-5M4 17v-5h5M6 7a7 7 0 0 1 12-2l2 3M4 16l2 3a7 7 0 0 0 12-2" />
      </>
    ),
    logout: (
      <>
        <path d="M9 4H4v16h5M14 8l4 4-4 4M9 12h10" />
      </>
    ),
    shield: (
      <>
        <path d="m12 3 8 3v6c0 5-8 9-8 9s-8-4-8-9V6l8-3Z" />
        <path d="m8 12 3 3 5-6" />
      </>
    ),
    link: (
      <>
        <path
          d="m10 13 4-4M8 16l-1 1a4 4 0 0 1-6-6l4-4a4 4 0 0 1 6 0M16 8l1-1a4 4 0 0 1 6 6l-4 4a4 4 0 0 1-6 0"
          transform="translate(0 -1)"
        />
      </>
    ),
  };
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      {paths[name] || paths.apps}
    </svg>
  );
}
function Logo() {
  return (
    <div className="brand">
      <span className="brand-mark">
        <Icon name="link" size={23} />
      </span>
      <span>
        tunnel<span className="brand-dot">.</span>
      </span>
    </div>
  );
}
function Field({
  label,
  children,
  hint,
}: {
  label: string;
  children: ReactNode;
  hint?: string;
}) {
  return (
    <label className="field">
      <span>{label}</span>
      {children}
      {hint && <small>{hint}</small>}
    </label>
  );
}
function Modal({
  title,
  children,
  onClose,
}: {
  title: string;
  children: ReactNode;
  onClose: () => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    dialog.current?.showModal();
  }, []);
  return (
    <dialog ref={dialog} className="modal" onCancel={onClose}>
      <div className="modal-head">
        <h2>{title}</h2>
        <button className="icon-button" onClick={onClose} aria-label="关闭">
          ×
        </button>
      </div>
      {children}
    </dialog>
  );
}

export default function Home() {
  const [authOptions, setAuthOptions] = useState({
    oidc: false,
    issuer: "",
    oidcLogout: false,
  });
  const [organizations, setOrganizations] = useState<
    { id: string; name: string }[]
  >([]);
  const [organization, setOrganization] = useState("default");
  const [inviteToken, setInviteToken] = useState("");
  const [memberAuth, setMemberAuth] = useState("local");
  const [user, setUser] = useState<User | null>(null);
  const [booting, setBooting] = useState(true);
  const [connected, setConnected] = useState(true);
  const [tab, setTab] = useState("overview");
  const [sites, setSites] = useState<Site[]>([]);
  const [services, setServices] = useState<Service[]>([]);
  const [users, setUsers] = useState<User[]>([]);
  const [editor, setEditor] = useState<Editor | null>(null);
  const [dialog, setDialog] = useState<Dialog>(null);
  const [enrollment, setEnrollment] = useState<Enrollment | null>(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [search, setSearch] = useState("");
  const [siteFilter, setSiteFilter] = useState("");
  const [probes, setProbes] = useState<Record<string, Probe>>({});
  const [diagnostic, setDiagnostic] = useState<{
    service: Service;
    result: Probe;
  } | null>(null);
  const [probing, setProbing] = useState("");
  const [opening, setOpening] = useState("");
  const root = user?.role === "admin";
  const permitted = (p: string) => !!root || !!user?.permissions?.includes(p);
  const admin =
    !!root ||
    !!user?.permissions?.some(
      (p) => p.endsWith(":manage") || p === "audit:read",
    );
  const sessionExpired = useCallback(() => {
    setUser(null);
    setTab("overview");
  }, []);

  const refresh = useCallback(
    async (u: User) => {
      const [rawSites, rawServices, rawUsers, health, orgs] = await Promise.all(
        [
          api<Site[]>("/sites"),
          api<Service[]>("/services"),
          u.role === "admin" || u.permissions?.includes("members:manage")
            ? api<User[]>("/users")
            : u.permissions?.includes("groups:manage")
              ? api<User[]>("/group-members")
              : Promise.resolve([]),
          api<{ reports: Probe[] }>("/status"),
          api<{ id: string; name: string }[]>("/organizations"),
        ],
      );
      const owned = (v: { organizationID?: string }) =>
        u.role !== "admin" || (v.organizationID || "default") === organization;
      const a = rawSites.filter(owned),
        b = rawServices.filter(owned),
        c = rawUsers.filter((v) => v.role === "admin" || owned(v));
      setOrganizations(orgs);
      setProbes((previous) => {
        const next: Record<string, Probe> = {};
        for (const service of b) {
          const previousReport = previous[service.id];
          const current =
            previousReport?.target === service.url &&
            previousReport.siteID === service.siteID
              ? previousReport
              : undefined;
          const report = health.reports.find((p) => p.serviceID === service.id);
          if (
            report &&
            (!current?.checkedAt ||
              (report.checkedAt || "") >= current.checkedAt)
          )
            next[service.id] = report;
          else if (current) next[service.id] = current;
        }
        return next;
      });
      setSites(a);
      setServices(b);
      setUsers(c);
      setConnected(true);
    },
    [organization],
  );
  useEffect(() => {
    const read = () => {
      const token = new URLSearchParams(window.location.hash.slice(1)).get(
        "invite",
      );
      if (token) {
        setInviteToken(token);
        window.history.replaceState(null, "", "/");
      }
    };
    read();
    window.addEventListener("hashchange", read);
    return () => window.removeEventListener("hashchange", read);
  }, []);
  useEffect(() => {
    api<typeof authOptions>("/auth/options")
      .then(setAuthOptions)
      .catch(() => {});
    if (
      new URLSearchParams(window.location.search).get("login_error") === "oidc"
    ) {
      setError("统一登录失败。请重试，或联系管理员核对身份绑定与账号状态。");
      window.history.replaceState(null, "", "/");
    }
    api<User>("/me")
      .then(async (u) => {
        setUser(u);
        await refresh(u);
      })
      .catch(() => {})
      .finally(() => setBooting(false));
  }, [refresh]);
  useEffect(() => {
    if (!user) return;
    const timer = setInterval(() => {
      refresh(user).catch((e) => {
        setConnected(false);
        if (e instanceof APIError && e.status === 401) setUser(null);
      });
    }, 10000);
    return () => clearInterval(timer);
  }, [user, refresh]);
  useEffect(() => {
    if (!notice) return;
    const timer = setTimeout(() => setNotice(""), 5000);
    return () => clearTimeout(timer);
  }, [notice]);

  async function login(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    setError("");
    setBusy(true);
    const data = new FormData(e.currentTarget);
    try {
      const u = await api<User>("/login", "POST", Object.fromEntries(data));
      setUser(u);
      await refresh(u);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  async function logout() {
    try {
      await api("/logout", "POST");
      setUser(null);
      setTab("overview");
      setSiteFilter("");
      setSites([]);
      setServices([]);
      setUsers([]);
      setEnrollment(null);
      setEditor(null);
      setProbes({});
      setError("");
    } catch (e) {
      setError((e as Error).message);
    }
  }
  async function create(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const data = new FormData(e.currentTarget);
    setBusy(true);
    setError("");
    try {
      if (dialog === "site") {
        const res = await api<{ site: Site; token: string }>("/sites", "POST", {
          name: data.get("name"),
          organizationID: root ? organization : user?.organizationID,
        });
        setEnrollment({ ...res.site, token: res.token });
      }
      if (dialog === "service")
        await api("/services", "POST", Object.fromEntries(data));
      if (dialog === "user")
        await api("/users", "POST", {
          username: data.get("username"),
          organizationID: root ? organization : user?.organizationID,
          password: data.get("password") || "",
          oidcSubject: data.get("oidcSubject") || "",
          siteIDs: data.getAll("siteIDs"),
          serviceIDs: data.getAll("serviceIDs"),
        });
      setDialog(null);
      if (user) await refresh(user);
      setNotice("已保存");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  async function remove(kind: string, id: string, name: string) {
    if (
      !window.confirm(
        `确定删除「${name}」？${kind === "sites" ? "关联应用也将删除，Agent 将断开连接。" : ""}`,
      )
    )
      return;
    try {
      await api(`/${kind}/${id}`, "DELETE");
      if (user) await refresh(user);
      setNotice("已删除");
    } catch (e) {
      setError((e as Error).message);
    }
  }
  async function rotate(site: Site) {
    if (
      !window.confirm("轮换后现有 Agent 将断开，请使用新令牌重新启动。继续？")
    )
      return;
    try {
      const res = await api<{ token: string }>(
        `/sites/${site.id}/rotate`,
        "POST",
      );
      setEnrollment({ ...site, token: res.token });
    } catch (e) {
      setError((e as Error).message);
    }
  }
  async function launch(service: Service) {
    setOpening(service.id);
    setError("");
    // Open synchronously so the browser does not block a window after the API round trip.
    const win = window.open("about:blank", "_blank");
    if (win) win.opener = null;
    if (!win) {
      setError("请允许此站点打开新窗口后重试。");
      setOpening("");
      return;
    }
    try {
      const res = await api<{ url: string }>(
        `/services/${service.id}/launch`,
        "POST",
      );
      win.location.href = res.url;
    } catch (e) {
      win.close();
      setError((e as Error).message);
    } finally {
      setOpening("");
    }
  }
  async function probe(service: Service) {
    setProbing(service.id);
    try {
      const res = await api<Probe>(`/services/${service.id}/probe`, "POST");
      setProbes((p) => ({ ...p, [service.id]: res }));
      if (!res.reachable) setDiagnostic({ service, result: res });
      else {
        setDiagnostic(null);
        setNotice(
          `HTTP 可达 · HTTP ${res.status}${res.healthy === false ? "，请检查业务状态" : ""}`,
        );
      }
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setProbing("");
    }
  }
  async function copy(value: string) {
    try {
      await navigator.clipboard.writeText(value);
      setNotice("已复制");
    } catch {
      setError("当前浏览器不支持自动复制，请手动选中文字复制。");
    }
  }
  const online = sites.filter((s) => s.online).length;
  const selectedSites = sites.filter((s) =>
    s.name.toLowerCase().includes(search.toLowerCase()),
  );
  const selectedServices = services.filter(
    (s) =>
      (!siteFilter || s.siteID === siteFilter) &&
      `${s.name} ${s.url} ${sites.find((x) => x.id === s.siteID)?.name}`
        .toLowerCase()
        .includes(search.toLowerCase()),
  );
  const titles: Record<string, [string, string]> = {
    overview: ["让每个内网，触手可及。", "一个入口，连接分散的站点与应用。"],
    sites: ["站点", "连接你的数据面，建立安全的出站隧道。"],
    services: ["应用", "从这里进入已授权的内网服务。"],
    users: ["访问权限", "为成员分配整站或单独应用访问权限。"],
    bulk: ["批量应用", "批量创建、迁移或删除同组织应用。"],
    organizations: ["组织与角色", "设置组织边界、管理能力和角色成员。"],
    administrators: ["管理员", "管理全局管理员，敏感变更需要再次验证密码。"],
    groups: ["用户组与邀请", "管理共享授权，并邀请成员自行设置密码。"],
    sessions: ["登录设备", "查看活跃会话，并撤销指定设备的访问。"],
    audit: ["审计日志", "查询访问、权限变更与健康状态变化。"],
  };

  if (booting)
    return (
      <div className="loading">
        <Logo />
        <p>正在连接控制面…</p>
      </div>
    );
  if (inviteToken)
    return (
      <InvitationLanding
        token={inviteToken}
        onCancel={() => setInviteToken("")}
        onAccepted={async (member) => {
          setUser(member);
          setInviteToken("");
          setTab("overview");
          await refresh(member);
        }}
      />
    );
  if (!user)
    return (
      <main className="login-page">
        <section className="login-story">
          <Logo />
          <div>
            <span className="eyebrow light">MULTI-SITE ACCESS GATEWAY</span>
            <h1>
              内网之间，
              <br />
              不再有距离。
            </h1>
            <p>
              一个控制面，连接每一个站点。
              <br />
              让访问安全、直接、井然有序。
            </p>
            <div className="network-art">
              <span>控制面</span>
              <i />
              <div>
                <b>站点 A</b>
                <b>站点 B</b>
                <b>站点 N</b>
              </div>
            </div>
          </div>
          <small>OUTBOUND ONLY · ENCRYPTED TUNNELS</small>
        </section>
        <section className="login-form">
          <div>
            <span className="eyebrow">WELCOME BACK</span>
            <h2>登录控制台</h2>
            <p className="muted">从这里，进入你的内网工作空间。</p>
            <form onSubmit={login}>
              <Field label="用户名">
                <input
                  name="username"
                  autoComplete="username"
                  required
                  autoFocus
                  placeholder="输入用户名"
                />
              </Field>
              <Field label="密码">
                <input
                  name="password"
                  type="password"
                  autoComplete="current-password"
                  required
                  placeholder="输入密码"
                />
              </Field>
              {error && (
                <p role="alert" className="form-error">
                  {error}
                </p>
              )}
              <button className="primary full" disabled={busy}>
                {busy ? "正在验证…" : "登录控制台"}
                <Icon name="arrow" size={17} />
              </button>
            </form>
            {authOptions.oidc && (
              <a className="secondary full oidc-login" href="/auth/oidc/start">
                使用统一身份登录
              </a>
            )}
            <p className="login-foot">
              <Icon name="shield" size={16} /> 仅可访问管理员授权的站点与应用
            </p>
          </div>
        </section>
      </main>
    );

  const addButton = (type: Dialog, label: string) =>
    permitted(
      type === "site"
        ? "sites:manage"
        : type === "service"
          ? "applications:manage"
          : "members:manage",
    ) && (
      <button
        className="primary"
        onClick={() => {
          setError("");
          setDialog(type);
        }}
      >
        <Icon name="plus" size={17} />
        {label}
      </button>
    );
  const siteCards = (list: Site[]) => (
    <div className="site-grid">
      {list.map((site) => (
        <article className="site-card" key={site.id}>
          <div className="card-top">
            <span className="site-icon">
              <Icon name="server" size={23} />
            </span>
            <span className={`status ${site.online ? "online" : ""}`}>
              <i />
              {site.online ? "已连接" : "离线"}
            </span>
          </div>
          <h3>{site.name}</h3>
          <code className="site-id">{site.id}</code>
          <div className="card-divider" />
          <div className="site-details">
            <span>
              {services.filter((s) => s.siteID === site.id).length} 个应用
            </span>
            <small>出站连接</small>
          </div>
          <div className="card-actions">
            <button
              className="text-button"
              onClick={() => {
                setSearch("");
                setSiteFilter(site.id);
                setTab("services");
              }}
            >
              查看应用 →
            </button>
            {permitted("sites:manage") && (
              <div>
                <button
                  className="subtle"
                  onClick={() => setEditor({ kind: "site", site })}
                >
                  编辑
                </button>
                <button className="subtle" onClick={() => rotate(site)}>
                  令牌
                </button>
                <button
                  className="subtle danger"
                  onClick={() => remove("sites", site.id, site.name)}
                >
                  删除
                </button>
              </div>
            )}
          </div>
        </article>
      ))}
    </div>
  );
  const appTable = (list: Service[]) => (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>应用名称</th>
            <th>所属站点</th>
            <th>内网地址</th>
            <th>数据通路</th>
            <th className="right">操作</th>
          </tr>
        </thead>
        <tbody>
          {list.map((service) => {
            const site = sites.find((s) => s.id === service.siteID);
            const p = probes[service.id];
            return (
              <tr key={service.id}>
                <td>
                  <span className="app-name">
                    <span className="app-icon">
                      <Icon name="apps" size={18} />
                    </span>
                    {service.name}
                  </span>
                </td>
                <td>
                  <span className={`dot ${site?.online ? "green" : ""}`} />
                  {site?.name || "未知站点"}
                </td>
                <td>
                  <code>{service.url}</code>
                </td>
                <td>
                  <button
                    className={`probe ${p ? (p.reachable && p.healthy !== false ? "good" : "bad") : ""}`}
                    onClick={() => probe(service)}
                    disabled={probing === service.id}
                    title={
                      p?.checkedAt
                        ? `探测于 ${new Date(p.checkedAt).toLocaleString()} · ${p.healthy ? "HEAD 返回 2xx/3xx" : p.reachable ? "HTTP 可达，请检查业务状态" : p.message}`
                        : p?.message
                    }
                  >
                    {probing === service.id
                      ? "探测中…"
                      : p
                        ? p.reachable
                          ? `HTTP ${p.status} · ${p.latencyMs}ms`
                          : `${diagnosisLabel(p.code)} · 重试`
                        : "检测连接"}
                  </button>
                  {p && !p.reachable && (
                    <button
                      className="diagnostic-link"
                      onClick={() => setDiagnostic({ service, result: p })}
                    >
                      诊断详情
                    </button>
                  )}
                </td>
                <td className="right">
                  <div className="row-actions">
                    <button
                      className="open-button"
                      disabled={
                        !site?.online ||
                        opening === service.id ||
                        service.accessible === false
                      }
                      onClick={() => launch(service)}
                    >
                      {opening === service.id ? "打开中…" : "打开"}
                      <Icon name="arrow" size={15} />
                    </button>
                    {permitted("applications:manage") && (
                      <button
                        className="subtle"
                        onClick={() => setEditor({ kind: "service", service })}
                      >
                        编辑
                      </button>
                    )}
                    {permitted("applications:manage") && (
                      <button
                        className="subtle danger"
                        onClick={() =>
                          remove("services", service.id, service.name)
                        }
                      >
                        删除
                      </button>
                    )}
                  </div>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );

  return (
    <div className="shell">
      <aside className="sidebar">
        <Logo />
        <div className="workspace">
          <span className="workspace-avatar">T</span>
          <div>
            <strong>我的工作空间</strong>
            <small>自托管控制面</small>
          </div>
          <span className="version">MVP</span>
        </div>
        {root && (
          <label className="field">
            <span>组织范围</span>
            <select
              aria-label="组织范围"
              value={organization}
              onChange={(e) => {
                setOrganization(e.target.value);
                setSiteFilter("");
                setEditor(null);
                setDialog(null);
                setProbes({});
              }}
            >
              {organizations.map((o) => (
                <option key={o.id} value={o.id}>
                  {o.name}
                </option>
              ))}
            </select>
          </label>
        )}
        <span className="nav-label">工作空间</span>
        <nav>
          {[
            ["overview", "grid", "概览"],
            ["sites", "server", "站点"],
            ["services", "apps", "应用"],
            ["sessions", "shield", "登录设备"],
            ...(permitted("members:manage")
              ? [["users", "users", "访问权限"]]
              : []),
            ...(permitted("groups:manage")
              ? [["groups", "users", "用户组与邀请"]]
              : []),
            ...(permitted("applications:manage")
              ? [["bulk", "apps", "批量应用"]]
              : []),
            ...(root
              ? [
                  ["organizations", "grid", "组织与角色"],
                  ["administrators", "shield", "管理员"],
                ]
              : []),
            ...(permitted("audit:read")
              ? [["audit", "shield", "审计日志"]]
              : []),
          ].map(([key, icon, label]) => (
            <button
              key={key}
              className={tab === key ? "active" : ""}
              onClick={() => {
                setTab(key);
                setSearch("");
                setSiteFilter("");
              }}
            >
              <Icon name={icon} />
              {label}
              {key === "sites" && <span>{sites.length}</span>}
              {key === "services" && <span>{services.length}</span>}
            </button>
          ))}
        </nav>
        <div className="sidebar-note">
          <Icon name="shield" />
          <strong>连接，由内向外</strong>
          <p>
            Agent 主动建立隧道
            <br />
            内网无需开放入站端口
          </p>
        </div>
        <div className="profile">
          <span className="avatar">
            {user.username.slice(0, 1).toUpperCase()}
          </span>
          <div>
            <strong>{user.username}</strong>
            <small>{admin ? "管理员" : "站点成员"}</small>
          </div>
          <button
            className="icon-button"
            aria-label="退出登录"
            onClick={logout}
          >
            <Icon name="logout" size={19} />
          </button>
        </div>
      </aside>
      <div className="main">
        <header className="topbar">
          <div>
            <span className="breadcrumb">工作空间</span>
            <span className="slash">/</span>
            <span>{tab === "overview" ? "概览" : titles[tab][0]}</span>
          </div>
          <div className="topbar-status">
            {user.sessionMethod === "oidc" && authOptions.oidcLogout && (
              <button
                className="subtle"
                onClick={async () => {
                  try {
                    const result = await api<{ url: string }>(
                      "/logout/identity",
                      "POST",
                    );
                    window.location.assign(result.url);
                  } catch (e) {
                    setError((e as Error).message);
                  }
                }}
              >
                退出统一身份
              </button>
            )}
            {user.passwordLogin && (
              <button
                className="subtle"
                onClick={() => {
                  setError("");
                  setEditor({ kind: "password" });
                }}
              >
                修改密码
              </button>
            )}
            <span className={`dot ${connected ? "green" : ""}`} />
            {connected ? "控制面已连接" : "控制面连接中断"}
            <button
              className="icon-button"
              title="刷新数据"
              aria-label="刷新数据"
              onClick={() =>
                refresh(user).catch((e) => {
                  if (e instanceof APIError && e.status === 401) setUser(null);
                  else setError(e.message);
                })
              }
            >
              <Icon name="refresh" size={17} />
            </button>
          </div>
        </header>
        <main className="content">
          <div className="page-heading">
            <div>
              <span className="eyebrow">
                {tab === "overview"
                  ? "YOUR NETWORK, CONNECTED"
                  : "WORKSPACE / " + tab.toUpperCase()}
              </span>
              <h1>{titles[tab][0]}</h1>
              <p>{titles[tab][1]}</p>
            </div>
            {tab === "sites"
              ? addButton("site", "接入站点")
              : tab === "services"
                ? addButton("service", "添加应用")
                : tab === "users"
                  ? addButton("user", "添加成员")
                  : tab === "overview"
                    ? addButton("site", "接入新站点")
                    : null}
          </div>
          {error && (
            <div className="error-banner" role="alert">
              <span>{error}</span>
              <button onClick={() => setError("")} aria-label="关闭错误">
                ×
              </button>
            </div>
          )}
          {notice && (
            <div className="toast" role="status">
              ✓ {notice}
            </div>
          )}
          {tab === "overview" && (
            <>
              <div className="stats">
                <div>
                  <span>已接入站点</span>
                  <strong>{sites.length.toString().padStart(2, "0")}</strong>
                  <small>分散部署，统一访问</small>
                  <Icon name="server" size={23} />
                </div>
                <div>
                  <span>在线站点</span>
                  <strong>
                    {online.toString().padStart(2, "0")}
                    <em className="live-indicator" />
                  </strong>
                  <small>
                    {sites.length
                      ? `${sites.length - online} 个站点当前离线`
                      : "等待第一个 Agent 连接"}
                  </small>
                  <Icon name="link" size={23} />
                </div>
                <div>
                  <span>可访问应用</span>
                  <strong>{services.length.toString().padStart(2, "0")}</strong>
                  <small>独立域名，隔离登录状态</small>
                  <Icon name="apps" size={23} />
                </div>
              </div>
              <div className="section-heading">
                <div>
                  <h2>
                    你的站点 <span>{sites.length}</span>
                  </h2>
                  <p>每一条连接，通向一个工作现场。</p>
                </div>
                <button className="text-button" onClick={() => setTab("sites")}>
                  全部站点 →
                </button>
              </div>
              {sites.length ? (
                siteCards(sites.slice(0, 3))
              ) : (
                <Empty
                  icon="server"
                  title="从接入第一个站点开始"
                  text={
                    admin
                      ? "创建站点、部署 Agent，再添加内网应用。"
                      : "请联系管理员为你分配站点权限。"
                  }
                  action={addButton("site", "接入站点")}
                />
              )}
              <div className="section-heading">
                <div>
                  <h2>应用快捷入口</h2>
                  <p>只需一次点击，即可进入内网服务。</p>
                </div>
                {addButton("service", "添加应用")}
              </div>
              {services.length ? (
                appTable(services.slice(0, 5))
              ) : (
                <Empty
                  icon="apps"
                  title="这里将是你的应用入口"
                  text="站点接入后，添加数据面平台或内网可达的其他 Web 服务。"
                />
              )}
              <div className="info-strip">
                <Icon name="shield" />
                <div>
                  <strong>隧道在线，还需要验证应用可达</strong>
                  <p>
                    使用“检测连接”从 Agent 所在内网实际探测 HTTP 服务，检查
                    ACL、DNS 与 TLS。
                  </p>
                </div>
                <span>端到端探测</span>
              </div>
            </>
          )}
          {(tab === "sites" || tab === "services") && (
            <>
              <div className="filterbar">
                <span>
                  {tab === "sites"
                    ? `${sites.length} 个站点 · ${online} 个在线`
                    : `${selectedServices.length} 个应用`}
                  {tab === "services" && siteFilter && (
                    <button
                      className="subtle"
                      onClick={() => setSiteFilter("")}
                    >
                      · {sites.find((s) => s.id === siteFilter)?.name} ×
                      清除筛选
                    </button>
                  )}
                </span>
                <input
                  aria-label="搜索"
                  placeholder={
                    tab === "sites" ? "搜索站点…" : "搜索应用、站点或地址…"
                  }
                  value={search}
                  onChange={(e) => setSearch(e.target.value)}
                />
              </div>
              {tab === "sites" ? (
                selectedSites.length ? (
                  siteCards(selectedSites)
                ) : (
                  <Empty
                    icon="server"
                    title="没有找到站点"
                    text="接入新的站点或调整搜索条件。"
                  />
                )
              ) : selectedServices.length ? (
                appTable(selectedServices)
              ) : (
                <Empty
                  icon="apps"
                  title="没有找到应用"
                  text="添加内网 HTTP / HTTPS 服务，或调整搜索条件。"
                />
              )}
            </>
          )}
          {tab === "bulk" && permitted("applications:manage") && (
            <BulkApplications
              key={root ? organization : user.organizationID || "default"}
              sites={sites}
              services={services}
              onChanged={() => refresh(user)}
            />
          )}
          {tab === "sessions" && (
            <SessionsPanel admin={!!root} onExpired={sessionExpired} />
          )}
          {tab === "groups" && permitted("groups:manage") && (
            <GroupsPanel
              key={root ? organization : user.organizationID || "default"}
              organizationID={
                root ? organization : user.organizationID || "default"
              }
              members={users.filter((m) => m.role !== "admin")}
              sites={sites}
              services={services}
            />
          )}
          {tab === "administrators" && root && (
            <AdministratorsPanel
              members={users}
              onChanged={() => user && refresh(user)}
              onExpired={sessionExpired}
            />
          )}
          {tab === "audit" && permitted("audit:read") && <AuditPanel />}
          {tab === "organizations" && root && (
            <OrganizationsPanel onChanged={() => refresh(user)} />
          )}
          {tab === "users" && permitted("members:manage") && (
            <>
              <div className="permission-note">
                <Icon name="shield" />
                <p>
                  全局管理员可管理全部组织；组织角色只在所属组织内生效。成员可通过整站授权或单独应用授权访问；Agent
                  仍会独立检查目标白名单。
                </p>
              </div>
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>成员</th>
                      <th>角色</th>
                      <th>站点 / 应用权限</th>
                      <th className="right">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {users.map((u) => (
                      <tr key={u.id}>
                        <td>
                          <strong>{u.username}</strong>
                          <div className="muted small">
                            {u.disabled ? "已禁用" : "正常"} ·{" "}
                            {u.identity
                              ? u.passwordLogin
                                ? "密码 + OIDC"
                                : "OIDC"
                              : "本地密码"}
                          </div>
                        </td>
                        <td>
                          <span className="role-badge">
                            {u.role === "admin" ? "管理员" : "成员"}
                          </span>
                        </td>
                        <td>
                          {u.role === "admin"
                            ? "全部站点"
                            : (u.siteIDs || [])
                                .map(
                                  (id) =>
                                    sites.find((s) => s.id === id)?.name ||
                                    "已删除站点",
                                )
                                .join("、") || "无整站授权"}
                          {u.role !== "admin" && !!u.serviceIDs?.length && (
                            <div className="muted small">
                              应用：
                              {u.serviceIDs
                                .map(
                                  (id) =>
                                    services.find((s) => s.id === id)?.name ||
                                    "已删除应用",
                                )
                                .join("、")}
                            </div>
                          )}
                        </td>
                        <td className="right">
                          {u.role !== "admin" && (root || !u.roleID) && (
                            <>
                              <button
                                className="subtle"
                                onClick={() =>
                                  setEditor({ kind: "grants", user: u })
                                }
                              >
                                编辑权限
                              </button>
                              {u.passwordLogin && (
                                <button
                                  className="subtle"
                                  onClick={() =>
                                    setEditor({ kind: "reset", user: u })
                                  }
                                >
                                  重置密码
                                </button>
                              )}
                              {(authOptions.oidc || u.identity) && (
                                <button
                                  className="subtle"
                                  onClick={() =>
                                    setEditor({ kind: "identity", user: u })
                                  }
                                >
                                  身份绑定
                                </button>
                              )}
                              <button
                                className="subtle"
                                onClick={async () => {
                                  if (
                                    !u.disabled &&
                                    !window.confirm(
                                      `禁用「${u.username}」将立即撤销全部登录和应用连接。继续？`,
                                    )
                                  )
                                    return;
                                  try {
                                    await api(
                                      `/users/${u.id}/disabled`,
                                      "PATCH",
                                      { disabled: !u.disabled },
                                    );
                                    await refresh(user);
                                    setNotice(
                                      u.disabled
                                        ? "已启用，请重新登录"
                                        : "已禁用并撤销访问",
                                    );
                                  } catch (e) {
                                    setError((e as Error).message);
                                  }
                                }}
                              >
                                {u.disabled ? "启用账号" : "禁用账号"}
                              </button>
                            </>
                          )}
                          {u.role !== "admin" && (root || !u.roleID) && (
                            <button
                              className="subtle danger"
                              onClick={() => remove("users", u.id, u.username)}
                            >
                              移除成员
                            </button>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </>
          )}
          <footer className="footer">
            <span>
              tunnel. <span>多站点访问网关</span>
            </span>
            <span>私有网络 · 统一访问</span>
          </footer>
        </main>
      </div>
      {editor && (
        <ResourceEditor
          editor={editor}
          oidcIssuer={authOptions.issuer}
          globalAdmin={!!root}
          sites={sites.filter(
            (v) =>
              !("user" in editor) ||
              (v.organizationID || "") === (editor.user.organizationID || ""),
          )}
          services={services.filter(
            (v) =>
              !("user" in editor) ||
              (v.organizationID || "") === (editor.user.organizationID || ""),
          )}
          onClose={() => setEditor(null)}
          onSaved={async (reauth) => {
            setEditor(null);
            if (reauth) {
              setUser(null);
              setTab("overview");
              setProbes({});
              setNotice("密码已修改，请重新登录");
            } else {
              await refresh(user);
              setNotice("已保存");
            }
          }}
        />
      )}
      {diagnostic && (
        <Modal
          title={`连接诊断 · ${diagnostic.service.name}`}
          onClose={() => setDiagnostic(null)}
        >
          <div className="diagnostic-status">
            {diagnosisLabel(diagnostic.result.code)}{" "}
            <code>{diagnostic.result.code || "unknown"}</code>
          </div>
          <p className="diagnostic-message" role="alert">
            {diagnostic.result.message || "未收到有效的 HTTP 响应。"}
          </p>
          <p className="form-tip">
            {diagnostic.result.hint ||
              "请查看 Server 与 Agent 日志中的目标地址及错误信息。"}
          </p>
          <dl className="diagnostic-fields">
            <dt>失败阶段</dt>
            <dd>{diagnostic.result.stage || "未知"}</dd>
            <dt>目标地址</dt>
            <dd>
              <code>{diagnostic.result.target || diagnostic.service.url}</code>
            </dd>
            <dt>站点 ID</dt>
            <dd>
              <code>
                {diagnostic.result.siteID || diagnostic.service.siteID}
              </code>
            </dd>
            <dt>耗时</dt>
            <dd>{diagnostic.result.latencyMs ?? "—"} ms</dd>
            {diagnostic.result.checkedAt && (
              <>
                <dt>探测时间</dt>
                <dd>
                  {new Date(diagnostic.result.checkedAt).toLocaleString()}
                </dd>
              </>
            )}
            {diagnostic.result.requestID && (
              <>
                <dt>Server 请求 ID</dt>
                <dd>
                  <code>{diagnostic.result.requestID}</code>
                </dd>
              </>
            )}
            {diagnostic.result.agentRequestID && (
              <>
                <dt>Agent 请求 ID</dt>
                <dd>
                  <code>{diagnostic.result.agentRequestID}</code>
                </dd>
              </>
            )}
            {!!diagnostic.result.resolvedIPs?.length && (
              <>
                <dt>解析到的 IP</dt>
                <dd>
                  <code>{diagnostic.result.resolvedIPs.join(", ")}</code>
                </dd>
              </>
            )}
          </dl>
          {!!diagnostic.result.attempts?.length && (
            <div className="diagnostic-attempts">
              <h3>Agent 连接尝试</h3>
              {diagnostic.result.attempts.map((attempt, index) => (
                <div key={`${attempt.ip}-${index}`}>
                  <code>{attempt.ip}</code>
                  <span>{attempt.allowed ? "白名单允许" : "白名单拒绝"}</span>
                  {attempt.error && <p>{attempt.error}</p>}
                </div>
              ))}
            </div>
          )}
          <p className="muted small">
            按请求 ID 检索两端日志。Agent 在容器中时，localhost / 127.0.0.1
            指向该容器自身。
          </p>
          <div className="modal-actions">
            <button
              className="secondary"
              onClick={() => copy(JSON.stringify(diagnostic.result, null, 2))}
            >
              复制诊断信息
            </button>
            <button
              className="primary"
              disabled={probing === diagnostic.service.id}
              onClick={() => probe(diagnostic.service)}
            >
              {probing === diagnostic.service.id ? "探测中…" : "重试"}
            </button>
          </div>
        </Modal>
      )}
      {dialog && (
        <Modal
          title={
            dialog === "site"
              ? "接入新站点"
              : dialog === "service"
                ? "添加内网应用"
                : "添加成员"
          }
          onClose={() => setDialog(null)}
        >
          <form onSubmit={create}>
            {dialog === "site" ? (
              <>
                <p className="muted">
                  为一个内网环境创建站点，再将 Agent
                  部署到可访问目标服务的机器。
                </p>
                <Field label="站点名称">
                  <input
                    name="name"
                    required
                    maxLength={100}
                    autoFocus
                    placeholder="例如：上海数据中心"
                  />
                </Field>
              </>
            ) : dialog === "service" ? (
              <>
                <Field label="应用名称">
                  <input
                    name="name"
                    required
                    maxLength={100}
                    autoFocus
                    placeholder="例如：数据面控制台"
                  />
                </Field>
                <Field label="所属站点">
                  <select name="siteID" required defaultValue="">
                    <option value="" disabled>
                      选择一个站点
                    </option>
                    {sites.map((s) => (
                      <option key={s.id} value={s.id}>
                        {s.name}
                      </option>
                    ))}
                  </select>
                </Field>
                <Field
                  label="内网地址"
                  hint="填写 HTTP / HTTPS 地址，可含已配置的子路径，例如 http://grafana.internal:3000/grafana/；DNS 在 Agent 侧解析。"
                >
                  <input
                    name="url"
                    type="url"
                    required
                    placeholder="http://10.10.1.20:3000"
                  />
                </Field>
                {root && (
                  <Field
                    label="自定义访问域名"
                    hint="可选；仅主机名，不含协议/端口。需要单独配置 DNS 和 TLS。"
                  >
                    <input
                      name="customDomain"
                      placeholder="reports.example.net"
                    />
                  </Field>
                )}
                <p className="form-tip">
                  添加应用后，请确保目标 IP 和端口位于 Agent
                  白名单内。路径前缀在公网保留，应用需配置相同子路径。
                </p>
              </>
            ) : (
              <>
                <Field label="用户名">
                  <input
                    name="username"
                    required
                    maxLength={100}
                    autoFocus
                    autoComplete="off"
                  />
                </Field>
                {authOptions.oidc && (
                  <Field label="登录方式">
                    <select
                      value={memberAuth}
                      onChange={(e) => setMemberAuth(e.target.value)}
                    >
                      <option value="local">本地密码</option>
                      <option value="oidc">OIDC 统一登录</option>
                    </select>
                  </Field>
                )}
                {memberAuth === "oidc" && authOptions.oidc ? (
                  <Field
                    label="OIDC Subject"
                    hint="从身份提供方获取 sub，精确区分大小写；不会按邮箱自动绑定。"
                  >
                    <input
                      name="oidcSubject"
                      required
                      maxLength={255}
                      autoComplete="off"
                    />
                    <small>Issuer：{authOptions.issuer}</small>
                  </Field>
                ) : (
                  <Field
                    label="初始密码"
                    hint="12–72 字节；请通过安全渠道交给成员。"
                  >
                    <input
                      name="password"
                      type="password"
                      minLength={12}
                      maxLength={72}
                      required
                      autoComplete="new-password"
                    />
                  </Field>
                )}
                <Grants sites={sites} services={services} />
              </>
            )}
            {error && (
              <p role="alert" className="form-error">
                {error}
              </p>
            )}
            <div className="modal-actions">
              <button
                type="button"
                className="secondary"
                onClick={() => setDialog(null)}
              >
                取消
              </button>
              <button
                className="primary"
                disabled={busy || (dialog === "service" && !sites.length)}
              >
                {busy ? "保存中…" : "创建"}
              </button>
            </div>
          </form>
        </Modal>
      )}
      {enrollment && (
        <Modal
          title={`${enrollment.name} · Agent 接入`}
          onClose={() => setEnrollment(null)}
        >
          <p className="form-tip">
            此令牌仅展示一次。请安全保存；关闭窗口后只能通过轮换生成新令牌。
          </p>
          <Field label="站点 ID">
            <code className="token-value">{enrollment.id}</code>
          </Field>
          <Field label="Agent 令牌">
            <code className="token-value">{enrollment.token}</code>
          </Field>
          <button className="secondary" onClick={() => copy(enrollment.token)}>
            复制令牌
          </button>
          <h3 className="config-label">
            1. 保存为 agent.local.json 并调整白名单
          </h3>
          <pre>
            {JSON.stringify(
              {
                serverURL: window.location.origin,
                siteID: enrollment.id,
                allow: [
                  { cidr: "127.0.0.0/8", ports: [3000] },
                  { cidr: "::1/128", ports: [3000] },
                ],
              },
              null,
              2,
            )}
          </pre>
          <h3 className="config-label">2. 在内网机器运行</h3>
          <pre>{`read -rs TUNNEL_AGENT_TOKEN\nexport TUNNEL_AGENT_TOKEN\n./tunnel agent --config agent.local.json`}</pre>
          <p className="muted small">
            粘贴令牌后回车。非本机连接需使用 HTTPS；私有 CA
            可通过系统信任库配置。
          </p>
          <div className="modal-actions">
            <button className="primary" onClick={() => setEnrollment(null)}>
              已安全保存
            </button>
          </div>
        </Modal>
      )}
    </div>
  );
}
function Empty({
  icon,
  title,
  text,
  action,
}: {
  icon: string;
  title: string;
  text: string;
  action?: ReactNode;
}) {
  return (
    <div className="empty">
      <span>
        <Icon name={icon} size={26} />
      </span>
      <h3>{title}</h3>
      <p>{text}</p>
      {action}
    </div>
  );
}

function diagnosisLabel(code?: string) {
  const labels: Record<string, string> = {
    acl_denied: "白名单拒绝",
    dns_failed: "DNS 解析失败",
    connection_refused: "连接被拒绝",
    network_unreachable: "网络不可达",
    tls_failed: "TLS 校验失败",
    timeout: "连接或响应超时",
    site_offline: "站点离线",
    invalid_target: "目标配置错误",
    http_failed: "HTTP 请求失败",
  };
  return labels[code || ""] || "不可达";
}
