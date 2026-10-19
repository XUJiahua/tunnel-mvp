"use client";
import { FormEvent, useEffect, useState } from "react";
import { api } from "./api";
type User = {
  id: string;
  username: string;
  role: "admin" | "viewer";
  siteIDs: string[];
  serviceIDs?: string[];
};
export function InvitationLanding({
  token,
  onAccepted,
  onCancel,
}: {
  token: string;
  onAccepted: (user: User) => Promise<void>;
  onCancel: () => void;
}) {
  const [info, setInfo] = useState<{
      username: string;
      expiresAt: string;
    } | null>(null),
    [error, setError] = useState(""),
    [busy, setBusy] = useState(false);
  useEffect(() => {
    api<{ username: string; expiresAt: string }>(
      "/invitations/inspect",
      "POST",
      { token },
    )
      .then(setInfo)
      .catch((e) => setError(e.message));
  }, [token]);
  async function accept(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const data = new FormData(e.currentTarget);
    if (data.get("password") !== data.get("confirm")) {
      setError("两次输入的密码不一致。");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const user = await api<User>("/invitations/accept", "POST", {
        token,
        password: data.get("password"),
      });
      await onAccepted(user);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <main className="invitation-page">
      <form className="group-editor" onSubmit={accept}>
        <h1>接受成员邀请</h1>
        {info && (
          <>
            <p>
              为 <strong>{info.username}</strong>{" "}
              设置密码。链接只能使用一次，接受后切换至该成员账号。
            </p>
            <p className="muted">
              有效期至 {new Date(info.expiresAt).toLocaleString()}
            </p>
            <label className="field">
              <span>设置密码</span>
              <input
                name="password"
                type="password"
                autoComplete="new-password"
                required
                minLength={12}
                maxLength={72}
              />
            </label>
            <label className="field">
              <span>确认密码</span>
              <input
                name="confirm"
                type="password"
                autoComplete="new-password"
                required
                minLength={12}
                maxLength={72}
              />
            </label>
          </>
        )}
        {error && (
          <p className="form-error" role="alert">
            {error}
          </p>
        )}
        <div className="modal-actions">
          <button type="button" className="secondary" onClick={onCancel}>
            返回登录
          </button>
          {info && (
            <button className="primary" disabled={busy}>
              {busy ? "正在加入…" : "接受邀请"}
            </button>
          )}
        </div>
      </form>
    </main>
  );
}
