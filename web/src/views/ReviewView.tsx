import { useState } from "react";
import { api } from "../api/client";
import type { DuplicateGroup, LibraryDetail, SimilarGroup } from "../api/types";
import { Thumb } from "../components/Thumb";
import { formatBytes, formatCount, formatDims, formatScore } from "../lib/format";
import { useSearchParams } from "../lib/router";
import { useApi } from "../lib/useApi";

const PAGE = 20;

type Kind = "duplicates" | "similar";

type Loaded =
  | { kind: "duplicates"; groups: DuplicateGroup[] }
  | { kind: "similar"; groups: SimilarGroup[] };

export function ReviewView({ libraryId, detail, onOpenPhoto }: {
  libraryId: string;
  detail: LibraryDetail;
  onOpenPhoto: (id: string) => void;
}) {
  const [params, setParams] = useSearchParams();
  const kind: Kind = params.get("kind") === "similar" ? "similar" : "duplicates";
  const [limit, setLimit] = useState(PAGE);

  // One request, for the visible tab only. The result is tagged with the kind
  // it was fetched for, because useApi keeps the previous data while the next
  // request loads -- and for a moment after switching tabs that previous data
  // is the OTHER kind's groups.
  const active = useApi(
    async (signal): Promise<Loaded> =>
      kind === "duplicates"
        ? { kind, groups: (await api.listDuplicates(libraryId, { limit }, { signal })).groups }
        : { kind, groups: (await api.listSimilar(libraryId, { limit }, { signal })).groups },
    [libraryId, limit, kind],
  );
  const loaded = active.data?.kind === kind ? active.data : undefined;
  const summary = kind === "duplicates" ? detail.duplicates : detail.similar;
  const groupCount = summary.groups;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Review</h1>
          <p>Groups of photos you may not need every copy of, with a suggestion for which to keep.</p>
        </div>
      </div>

      <div className="notice">
        <span>
          <strong>Suggestions only.</strong> This app cannot delete anything. Use “Copy other paths” to take a
          group's list into your own file manager, and decide there.
        </span>
      </div>

      <div className="tabs" role="tablist">
        <button
          role="tab"
          className="tab"
          aria-selected={kind === "duplicates"}
          onClick={() => {
            setLimit(PAGE);
            setParams({ kind: undefined });
          }}
        >
          Exact duplicates<span className="count">{detail.duplicates.groups}</span>
        </button>
        <button
          role="tab"
          className="tab"
          aria-selected={kind === "similar"}
          onClick={() => {
            setLimit(PAGE);
            setParams({ kind: "similar" });
          }}
        >
          Near-duplicates<span className="count">{detail.similar.groups}</span>
        </button>
      </div>

      <div className="stat-grid" style={{ marginBottom: 20 }}>
        <div className="card stat">
          <span className="stat-label">Could be reclaimed</span>
          <span className="stat-value">{formatBytes(summary.reclaimable_bytes)}</span>
          <span className="stat-sub">if only the suggested photo in each group were kept</span>
        </div>
        <div className="card stat">
          <span className="stat-label">Groups</span>
          <span className="stat-value">{groupCount}</span>
          <span className="stat-sub">
            {kind === "duplicates"
              ? formatCount(detail.duplicates.duplicate_files, "file") + " involved"
              : formatCount(detail.similar.similar_photos, "photo") + " involved"}
          </span>
        </div>
      </div>

      {active.error && (
        <div className="notice notice-danger" role="alert">
          <strong>Could not load groups.</strong> {active.error.message}
        </div>
      )}

      {!loaded && !active.error && <p className="muted">Loading…</p>}

      {loaded?.kind === "duplicates" &&
        (loaded.groups.length === 0 ? (
          <Empty text="No exact duplicates. Every file in this library is unique." />
        ) : (
          loaded.groups.map((g) => <DuplicateCard key={g.id} group={g} onOpen={onOpenPhoto} />)
        ))}

      {loaded?.kind === "similar" &&
        (loaded.groups.length === 0 ? (
          <Empty text="No near-duplicates found at the current similarity threshold." />
        ) : (
          loaded.groups.map((g) => <SimilarCard key={g.id} group={g} onOpen={onOpenPhoto} />)
        ))}

      {loaded && limit < groupCount && (
        <div className="load-more">
          {/* Groups are few and small, and the API caps this listing at 200,
              so growing the limit is safe here in a way it is not for photos. */}
          <button className="btn" onClick={() => setLimit((n) => Math.min(n + PAGE, 200))} disabled={active.loading || limit >= 200}>
            {active.loading && <span className="spinner" aria-hidden="true" />}
            Show more groups
          </button>
        </div>
      )}
    </>
  );
}

function Empty({ text }: { text: string }) {
  return (
    <div className="card empty">
      <h3>Nothing to review</h3>
      <p>{text}</p>
    </div>
  );
}

/**
 * Copies the paths of every photo except the suggested keeper.
 *
 * This is the only "action" the review screen offers, and it is deliberately
 * one that does nothing to the files. The user takes the list to their own
 * tools. The clipboard write is local to the browser; nothing leaves the
 * machine.
 */
function CopyOthers({ paths }: { paths: string[] }) {
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");
  async function copy() {
    try {
      await navigator.clipboard.writeText(paths.join("\n"));
      setState("copied");
    } catch {
      // Clipboard access needs a secure context and permission. localhost
      // qualifies, but a LAN address over plain HTTP does not.
      setState("failed");
    }
    setTimeout(() => setState("idle"), 2000);
  }
  return (
    <button className="btn btn-sm" onClick={copy} disabled={paths.length === 0}>
      {state === "copied" ? "Copied" : state === "failed" ? "Clipboard unavailable" : `Copy other paths (${paths.length})`}
    </button>
  );
}

function DuplicateCard({ group: g, onOpen }: { group: DuplicateGroup; onOpen: (id: string) => void }) {
  const others = g.photos.filter((p) => !p.suggested_keep).map((p) => p.relative_path);
  return (
    <section className="card group">
      <div className="group-head">
        <div className="group-title">
          {formatCount(g.photo_count, "identical copy", "identical copies")}
          <span className="badge">{formatBytes(g.reclaimable_bytes)} reclaimable</span>
          <span className="faint mono" title={g.sha256}>{g.sha256.slice(0, 12)}</span>
        </div>
        <CopyOthers paths={others} />
      </div>
      <p className="faint" style={{ margin: "-6px 0 14px", fontSize: 12.5 }}>
        Byte-for-byte identical, so the choice is only which location to keep: the shallowest folder, then the
        shortest name.
      </p>
      <div className="group-members">
        {g.photos.map((p) => (
          <div key={p.photo_id} className={`member ${p.suggested_keep ? "keep" : ""}`}>
            <button className="thumb-btn" onClick={() => onOpen(p.photo_id)} aria-label={`Details for ${p.relative_path}`}>
              <span style={{ position: "relative", display: "block" }}>
                {p.suggested_keep && (
                  <span className="tile-badges"><span className="badge badge-keep">Suggested keep</span></span>
                )}
                <Thumb photoId={p.photo_id} alt={p.relative_path} available />
              </span>
            </button>
            <span className="member-path mono">{p.relative_path}</span>
          </div>
        ))}
      </div>
    </section>
  );
}

function SimilarCard({ group: g, onOpen }: { group: SimilarGroup; onOpen: (id: string) => void }) {
  const others = g.photos.filter((p) => !p.suggested_keep).map((p) => p.relative_path);
  return (
    <section className="card group">
      <div className="group-head">
        <div className="group-title">
          {formatCount(g.photo_count, "similar photo")}
          <span className="badge">{formatBytes(g.reclaimable_bytes)} reclaimable</span>
          {g.chained && (
            <span
              className="badge badge-warn"
              title={`Members were linked through intermediates, so the two most different are ${g.max_distance} bits apart -- past the threshold of ${g.threshold}.`}
            >
              Chained — check carefully
            </span>
          )}
        </div>
        <CopyOthers paths={others} />
      </div>
      <p className="faint" style={{ margin: "-6px 0 14px", fontSize: 12.5 }}>
        Visually similar but not identical. The suggestion prefers higher measured quality, then resolution, then
        file size. Distance is bits differing from the suggested photo, out of 64.
      </p>
      <div className="group-members">
        {g.photos.map((p) => (
          <div key={p.photo_id} className={`member ${p.suggested_keep ? "keep" : ""}`}>
            <button className="thumb-btn" onClick={() => onOpen(p.photo_id)} aria-label={`Details for ${p.relative_path}`}>
              <span style={{ position: "relative", display: "block" }}>
                {p.suggested_keep && (
                  <span className="tile-badges"><span className="badge badge-keep">Suggested keep</span></span>
                )}
                <Thumb photoId={p.photo_id} alt={p.relative_path} available />
              </span>
            </button>
            <span className="member-path mono">{p.relative_path}</span>
            <span className="member-facts">
              <span>{formatDims(p.width, p.height)}</span>
              <span>{formatBytes(p.file_size_bytes)}</span>
              <span title="Measured technical quality">Q {formatScore(p.quality_score)}</span>
              {!p.suggested_keep && <span title="Hamming distance from the suggested photo">Δ {p.distance}</span>}
            </span>
          </div>
        ))}
      </div>
    </section>
  );
}
