import { useEffect, useRef, useState, type FormEvent } from "react";
import { ApiError, api } from "../api/client";
import type { Library } from "../api/types";
import { formatCount, formatDateTime } from "../lib/format";
import { navigate } from "../lib/router";
import { useApi } from "../lib/useApi";
import { Link } from "../components/Link";

type Step = "idle" | "creating" | "scanning" | "queueing";

const STEP_LABEL: Record<Step, string> = {
  idle: "",
  creating: "Registering folder…",
  scanning: "Scanning for photos…",
  queueing: "Queueing analysis…",
};

/** Resolves after `ms`, or rejects as soon as `signal` aborts. */
function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) return reject(signal.reason);
    const t = setTimeout(resolve, ms);
    signal.addEventListener("abort", () => {
      clearTimeout(t);
      reject(signal.reason);
    }, { once: true });
  });
}

export function LibrariesView() {
  const libs = useApi((signal) => api.listLibraries({ signal }), []);
  const [path, setPath] = useState("/photos");
  const [step, setStep] = useState<Step>("idle");
  const [error, setError] = useState<string>();

  // Aborted when the view unmounts, so navigating away mid-scan stops the wait
  // loop instead of leaving it polling for a page nobody is looking at. The
  // scan itself runs on the server and carries on regardless.
  const flow = useRef<AbortController>(null);
  useEffect(() => () => flow.current?.abort(), []);

  // Adding a folder is three API calls with a wait in the middle, because the
  // pipeline can only be queued for photos that exist, and scanning -- which
  // creates them -- is asynchronous. Queueing before the scan finished would
  // enqueue jobs for whatever fraction had been discovered so far and silently
  // leave the rest unanalysed.
  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    flow.current?.abort();
    const controller = new AbortController();
    flow.current = controller;
    const { signal } = controller;
    try {
      setStep("creating");
      const lib = await api.createLibrary(path.trim());

      setStep("scanning");
      try {
        await api.scanLibrary(lib.id);
      } catch (err) {
        // 409: a scan is already running for this folder. Not a failure --
        // wait for that one instead of starting another.
        if (!(err instanceof ApiError && err.status === 409)) throw err;
      }
      for (;;) {
        const detail = await api.getLibrary(lib.id, { signal });
        if (detail.library.scan_state === "complete") break;
        await sleep(750, signal);
      }

      setStep("queueing");
      await api.processLibrary(lib.id);
      navigate(`/l/${lib.id}`);
    } catch (err) {
      if (signal.aborted) return; // left the page; nothing to report
      setError(err instanceof Error ? err.message : String(err));
      setStep("idle");
    }
  }

  const busy = step !== "idle";
  const list: Library[] = libs.data?.libraries ?? [];

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Libraries</h1>
          <p>Folders you have asked this tool to analyse. Nothing outside them is ever read.</p>
        </div>
      </div>

      <div className="notice">
        <span>
          <strong>Read-only.</strong> Analysis never modifies, moves or deletes a photo. Every cleanup
          action in this app is a suggestion for you to review.
        </span>
      </div>

      <form className="card add-library" onSubmit={onSubmit}>
        <label htmlFor="library-path" style={{ fontWeight: 600 }}>
          Add a folder
        </label>
        <div className="row">
          <input
            id="library-path"
            className="input mono"
            value={path}
            onChange={(e) => setPath(e.target.value)}
            placeholder="/photos/2024"
            disabled={busy}
            spellCheck={false}
            autoComplete="off"
          />
          <button className="btn btn-primary" type="submit" disabled={busy || !path.trim()}>
            {busy && <span className="spinner" aria-hidden="true" />}
            {busy ? STEP_LABEL[step] : "Scan and analyse"}
          </button>
        </div>
        <p className="field-help">
          A path inside the server's photo root. In Docker that is <code>/photos</code>, the folder set as{" "}
          <code>HOST_PHOTOS_DIR</code>.
        </p>
        {error && (
          <p className="error-text" role="alert">
            {error}
          </p>
        )}
      </form>

      <h2>Your libraries</h2>
      {libs.error && (
        <div className="notice notice-danger">
          <strong>Could not reach the API.</strong> {libs.error.message}
        </div>
      )}
      {libs.loading && !libs.data && <p className="muted">Loading…</p>}
      {libs.data && list.length === 0 && (
        <div className="card empty">
          <h3>No libraries yet</h3>
          <p>Add a folder above to scan it.</p>
        </div>
      )}
      <div className="library-list">
        {list.map((lib) => (
          <Link key={lib.id} className="card library-item" to={`/l/${lib.id}`}>
            <div style={{ minWidth: 0 }}>
              <div className="name">{lib.name}</div>
              <div className="muted mono" style={{ overflow: "hidden", textOverflow: "ellipsis" }}>
                {lib.root_path}
              </div>
            </div>
            <div style={{ textAlign: "right", flex: "none" }}>
              {lib.scan_state === "scanning" ? (
                <span className="badge badge-accent">Scanning</span>
              ) : lib.scan_state === "never_scanned" ? (
                <span className="badge">Not scanned</span>
              ) : (
                <div className="faint" style={{ fontSize: 12.5 }}>
                  Scanned {formatDateTime(lib.last_scan_finished_at)}
                </div>
              )}
              {lib.photo_count !== undefined && (
                <div className="muted">{formatCount(lib.photo_count, "photo")}</div>
              )}
            </div>
          </Link>
        ))}
      </div>
    </>
  );
}
