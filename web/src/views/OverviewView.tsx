import { useEffect, useRef, useState } from "react";
import { api } from "../api/client";
import type { JobCounts, LibraryDetail } from "../api/types";
import { Link } from "../components/Link";
import { formatBytes, formatCount, formatFlag, formatScore } from "../lib/format";
import { useApi } from "../lib/useApi";
import { usePolling } from "../lib/usePolling";

// Pipeline stages in the order they are worth reading, with plain names. The
// queue's own type names are an implementation detail.
const STAGES: { type: string; label: string }[] = [
  { type: "EXTRACT_METADATA", label: "Metadata" },
  { type: "COMPUTE_FILE_HASH", label: "Exact fingerprints" },
  { type: "COMPUTE_PERCEPTUAL_HASH", label: "Visual fingerprints" },
  { type: "ANALYZE_QUALITY", label: "Quality analysis" },
  { type: "GENERATE_THUMBNAIL", label: "Thumbnails" },
  { type: "BUILD_DUPLICATE_GROUPS", label: "Group duplicates" },
  { type: "BUILD_SIMILAR_GROUPS", label: "Group near-duplicates" },
  { type: "BUILD_CLUSTERS", label: "Group into events" },
];

export function OverviewView({ libraryId, detail, onChanged }: {
  libraryId: string;
  detail: LibraryDetail;
  onChanged: () => void;
}) {
  const jobs = useApi((signal) => api.getJobs(libraryId, { signal }), [libraryId]);
  const [queueError, setQueueError] = useState<string>();
  const [queueing, setQueueing] = useState(false);

  const total = jobs.data?.total;
  const active = !!total && total.pending + total.running > 0;

  // Poll while work is outstanding. When the last job finishes, refresh the
  // library summary once so the stat cards show the finished result -- they
  // are computed from the database, not from the queue, and do not update on
  // their own.
  //
  // In an effect, not during render: onChanged updates the PARENT's state, and
  // React forbids updating one component while rendering another.
  const wasActive = useRef(false);
  useEffect(() => {
    if (wasActive.current && !active) onChanged();
    wasActive.current = active;
  }, [active, onChanged]);
  usePolling(async () => {
    jobs.reload();
    onChanged();
  }, 1500, active);

  async function runAnalysis() {
    setQueueError(undefined);
    setQueueing(true);
    try {
      await api.processLibrary(libraryId);
      jobs.reload();
    } catch (err) {
      setQueueError(err instanceof Error ? err.message : String(err));
    } finally {
      setQueueing(false);
    }
  }

  const d = detail;
  const photoCount = d.library.photo_count ?? 0;
  const deadJobs = total?.dead ?? 0;
  const flags = Object.entries(d.quality.by_flag).sort((a, b) => b[1] - a[1]);

  return (
    <>
      <div className="page-head">
        <div>
          <h1>{d.library.name}</h1>
          <p className="mono">{d.library.root_path}</p>
        </div>
        <button className="btn" onClick={runAnalysis} disabled={queueing || active}>
          {(queueing || active) && <span className="spinner" aria-hidden="true" />}
          {active ? "Analysing…" : "Re-run analysis"}
        </button>
      </div>

      {queueError && (
        <div className="notice notice-danger" role="alert">
          <strong>Could not queue analysis.</strong> {queueError}
        </div>
      )}
      {deadJobs > 0 && (
        <div className="notice notice-danger">
          <span>
            <strong>{formatCount(deadJobs, "job")} failed permanently</strong> after exhausting retries. Results
            below cover everything else; see the API logs for the failures.
          </span>
        </div>
      )}

      <div className="stat-grid">
        <Link className="card stat" to={`/l/${libraryId}/photos`}>
          <span className="stat-label">Photos</span>
          <span className="stat-value">{photoCount.toLocaleString("en-US")}</span>
          <span className="stat-sub">
            {formatCount(d.metadata.with_gps, "with GPS", "with GPS")} ·{" "}
            {d.metadata.failed > 0 ? `${d.metadata.failed} unreadable` : "all readable"}
          </span>
        </Link>
        <Link className="card stat" to={`/l/${libraryId}/review`}>
          <span className="stat-label">Exact duplicates</span>
          <span className="stat-value">{formatBytes(d.duplicates.reclaimable_bytes)}</span>
          <span className="stat-sub">
            reclaimable across {formatCount(d.duplicates.groups, "group")}
          </span>
        </Link>
        <Link className="card stat" to={`/l/${libraryId}/review?kind=similar`}>
          <span className="stat-label">Near-duplicates</span>
          <span className="stat-value">{formatBytes(d.similar.reclaimable_bytes)}</span>
          <span className="stat-sub">
            reclaimable across {formatCount(d.similar.groups, "group")}
          </span>
        </Link>
        <Link className="card stat" to={`/l/${libraryId}/photos?has_flags=true&sort=quality`}>
          <span className="stat-label">Worth a second look</span>
          <span className="stat-value">{d.quality.flagged.toLocaleString("en-US")}</span>
          <span className="stat-sub">
            flagged · mean quality {formatScore(d.quality.mean_quality_score)}
          </span>
        </Link>
        <Link className="card stat" to={`/l/${libraryId}/timeline`}>
          <span className="stat-label">Events</span>
          <span className="stat-value">{d.clusters.clusters.toLocaleString("en-US")}</span>
          <span className="stat-sub">
            {d.clusters.undated_photos > 0
              ? `${d.clusters.undated_photos} undated photos not placed`
              : `largest has ${formatCount(d.clusters.largest_cluster_photos, "photo")}`}
          </span>
        </Link>
      </div>

      <h2>Pipeline</h2>
      <div className="card pipeline">
        {!jobs.data && !jobs.error && <p className="muted">Loading…</p>}
        {jobs.error && <p className="error-text">{jobs.error.message}</p>}
        {jobs.data &&
          (Object.keys(jobs.data.by_type).length === 0 ? (
            <p className="muted">Nothing has been queued for this library yet.</p>
          ) : (
            STAGES.filter((s) => jobs.data!.by_type[s.type]).map((s) => (
              <StageRow key={s.type} label={s.label} counts={jobs.data!.by_type[s.type]!} />
            ))
          ))}
      </div>

      {flags.length > 0 && (
        <>
          <h2>Quality flags</h2>
          <div className="chips">
            {flags.map(([flag, n]) => (
              <Link key={flag} className="chip" to={`/l/${libraryId}/photos?flag=${flag}&sort=quality`}>
                {formatFlag(flag)}
                <span className="count">{n}</span>
              </Link>
            ))}
          </div>
          <p className="faint" style={{ fontSize: 12.5, margin: 0 }}>
            Flags describe pixels, not merit. A photo can be deliberately dark, soft or high-key.
          </p>
        </>
      )}
    </>
  );
}

function StageRow({ label, counts }: { label: string; counts: JobCounts }) {
  const pct = (n: number) => (counts.total ? (n / counts.total) * 100 : 0);
  const outstanding = counts.pending + counts.running;
  return (
    <div className="pipeline-row">
      <span className="pipeline-label">{label}</span>
      <div
        className="bar"
        role="progressbar"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={counts.total}
        aria-valuenow={counts.succeeded}
      >
        <span className="done" style={{ width: `${pct(counts.succeeded)}%` }} />
        <span className="running" style={{ width: `${pct(counts.running)}%` }} />
        <span className="dead" style={{ width: `${pct(counts.dead)}%` }} />
      </div>
      <span className="pipeline-count">
        {outstanding > 0
          ? `${counts.succeeded} / ${counts.total}`
          : counts.dead > 0
            ? `${counts.dead} failed`
            : `${counts.total} done`}
      </span>
    </div>
  );
}
