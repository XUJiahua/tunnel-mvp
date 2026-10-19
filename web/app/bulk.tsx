"use client";
import { FormEvent, useState } from "react";
import { api } from "./api";
type Site = { id: string; name: string };
type Service = { id: string; name: string; siteID: string; url: string };
export function BulkApplications({
  sites,
  services,
  onChanged,
}: {
  sites: Site[];
  services: Service[];
  onChanged: () => void;
}) {
  const [selected, setSelected] = useState<string[]>([]),
    [destination, setDestination] = useState(sites[0]?.id || "");
  const [rows, setRows] = useState([
      { name: "", url: "", siteID: sites[0]?.id || "" },
    ]),
    [error, setError] = useState(""),
    [notice, setNotice] = useState(""),
    [busy, setBusy] = useState(false);
  const ids = selected.filter((id) => services.some((s) => s.id === id));
  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api("/services/bulk", "POST", { services: rows });
      setRows([{ name: "", url: "", siteID: sites[0]?.id || "" }]);
      onChanged();
      setNotice(`已创建 ${rows.length} 个应用。`);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  async function operate(move: boolean) {
    if (
      !window.confirm(
        `${move ? "迁移" : "删除"}所选 ${ids.length} 个应用？将立即关闭这些应用的现有连接，用户需要重新打开。${move ? "目标站点的 Agent 必须允许原目标地址。" : "关联应用授权也会移除。"}`,
      )
    )
      return;
    setBusy(true);
    setError("");
    try {
      await api(
        move ? "/services/bulk-move" : "/services/bulk-delete",
        "POST",
        { ids, ...(move ? { siteID: destination } : {}) },
      );
      setSelected([]);
      onChanged();
      setNotice(move ? "应用已迁移，请重新验证目标连接。" : "应用已删除。");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      <p className="permission-note">
        每次最多 100
        个应用，全部成功后一起保存。迁移只在同一组织内进行，保留应用 ID
        与单应用授权，整站授权以迁移后的站点为准。
      </p>
      {error && (
        <p className="form-error" role="alert">
          {error}
        </p>
      )}
      {notice && <p role="status">{notice}</p>}
      <form className="group-form" onSubmit={submit}>
        <h3>批量添加应用</h3>
        {rows.map((row, i) => (
          <fieldset className="grants" key={i}>
            <legend>应用 {i + 1}</legend>
            <label className="field">
              <span>批量应用名称 {i + 1}</span>
              <input
                required
                maxLength={100}
                value={row.name}
                onChange={(e) =>
                  setRows(
                    rows.map((r, j) =>
                      j === i ? { ...r, name: e.target.value } : r,
                    ),
                  )
                }
              />
            </label>
            <label className="field">
              <span>批量内网地址 {i + 1}</span>
              <input
                type="url"
                required
                placeholder="http://internal:3000/grafana/"
                value={row.url}
                onChange={(e) =>
                  setRows(
                    rows.map((r, j) =>
                      j === i ? { ...r, url: e.target.value } : r,
                    ),
                  )
                }
              />
            </label>
            <label className="field">
              <span>批量所属站点 {i + 1}</span>
              <select
                aria-label={`批量所属站点 ${i + 1}`}
                required
                value={row.siteID}
                onChange={(e) =>
                  setRows(
                    rows.map((r, j) =>
                      j === i ? { ...r, siteID: e.target.value } : r,
                    ),
                  )
                }
              >
                <option value="">选择站点</option>
                {sites.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name}
                  </option>
                ))}
              </select>
            </label>
            {rows.length > 1 && (
              <button
                type="button"
                className="subtle danger"
                onClick={() => setRows(rows.filter((_, j) => j !== i))}
              >
                移除此行
              </button>
            )}
          </fieldset>
        ))}
        <button
          className="secondary"
          type="button"
          disabled={busy || rows.length >= 100}
          onClick={() =>
            setRows([
              ...rows,
              { name: "", url: "", siteID: sites[0]?.id || "" },
            ])
          }
        >
          增加一行
        </button>
        <button className="primary" disabled={busy || !sites.length}>
          批量创建
        </button>
      </form>
      <h3>批量迁移与删除</h3>
      <label className="field">
        <span>迁移到站点</span>
        <select
          aria-label="迁移到站点"
          value={destination}
          onChange={(e) => setDestination(e.target.value)}
        >
          <option value="">选择站点</option>
          {sites.map((s) => (
            <option key={s.id} value={s.id}>
              {s.name}
            </option>
          ))}
        </select>
      </label>
      <div className="row-actions">
        <span>已选择 {ids.length} 个应用</span>
        <button
          className="secondary"
          disabled={busy || !ids.length || ids.length > 100 || !destination}
          onClick={() => void operate(true)}
        >
          迁移所选
        </button>
        <button
          className="secondary danger"
          disabled={busy || !ids.length || ids.length > 100}
          onClick={() => void operate(false)}
        >
          删除所选
        </button>
      </div>
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>选择</th>
              <th>应用</th>
              <th>站点 / 内网地址</th>
            </tr>
          </thead>
          <tbody>
            {services.map((s) => (
              <tr key={s.id}>
                <td>
                  <input
                    type="checkbox"
                    aria-label={`选择应用 ${s.name}`}
                    checked={ids.includes(s.id)}
                    onChange={(e) =>
                      setSelected(
                        e.target.checked
                          ? [...ids, s.id]
                          : ids.filter((id) => id !== s.id),
                      )
                    }
                  />
                </td>
                <td>{s.name}</td>
                <td>
                  {sites.find((site) => site.id === s.siteID)?.name}
                  <div>
                    <code>{s.url}</code>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  );
}
