import { useState } from "react";
import { api } from "../api/client";
import type { Cluster, LibraryDetail } from "../api/types";
import { Thumb } from "../components/Thumb";
import { basename, formatCount, formatDate, formatMeters, formatSpan, formatTime } from "../lib/format";
import { useApi } from "../lib/useApi";

const PAGE = 25;

export function TimelineView({ libraryId, detail, onOpenPhoto }: {
  libraryId: string;
  detail: LibraryDetail;
  onOpenPhoto: (id: string) => void;
}) {
  const [limit, setLimit] = useState(PAGE);
  const result = useApi((signal) => api.listClusters(libraryId, { limit }, { signal }), [libraryId, limit]);
  const s = detail.clusters;
  const clusters: Cluster[] = result.data?.clusters ?? [];

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Timeline</h1>
          <p>
            Photos grouped into events by when they were taken and, where the camera recorded it, where. Newest
            first.
          </p>
        </div>
      </div>

      <div className="stat-grid" style={{ marginBottom: 20 }}>
        <div className="card stat">
          <span className="stat-label">Events</span>
          <span className="stat-value">{s.clusters}</span>
          <span className="stat-sub">{formatCount(s.clustered_photos, "photo")} placed</span>
        </div>
        <div className="card stat">
          <span className="stat-label">With location</span>
          <span className="stat-value">{s.located_photos}</span>
          <span className="stat-sub">the rest joined an event on time alone</span>
        </div>
        {/* Reported, not hidden: a photo with no date is a fact about the file,
            and dropping it silently would make the counts stop adding up. */}
        <div className="card stat">
          <span className="stat-label">Not placed</span>
          <span className="stat-value">{s.undated_photos}</span>
          <span className="stat-sub">no timestamp of any kind</span>
        </div>
      </div>

      {s.low_confidence_clusters > 0 && (
        <div className="notice">
          <span>
            <strong>{formatCount(s.low_confidence_clusters, "event")} dated only from file times.</strong> Copying a
            folder stamps every file within seconds, so these may be groups of files that were copied together
            rather than photos taken together.
          </span>
        </div>
      )}

      {result.error && (
        <div className="notice notice-danger" role="alert">
          <strong>Could not load the timeline.</strong> {result.error.message}
        </div>
      )}
      {!result.data && !result.error && <p className="muted">Loading…</p>}
      {result.data && clusters.length === 0 && (
        <div className="card empty">
          <h3>No events yet</h3>
          <p>Events appear once metadata extraction has finished.</p>
        </div>
      )}

      {clusters.map((c) => (
        <EventCard key={c.id} cluster={c} onOpen={onOpenPhoto} />
      ))}

      {result.data && limit < s.clusters && (
        <div className="load-more">
          <button
            className="btn"
            onClick={() => setLimit((n) => Math.min(n + PAGE, 500))}
            disabled={result.loading || limit >= 500}
          >
            {result.loading && <span className="spinner" aria-hidden="true" />}
            Show earlier events
          </button>
        </div>
      )}
    </>
  );
}

function EventCard({ cluster: c, onOpen }: { cluster: Cluster; onOpen: (id: string) => void }) {
  const sameDay = formatDate(c.started_at) === formatDate(c.ended_at);
  return (
    <section className="card event">
      <div className="event-head">
        <div>
          <div className="event-when">
            {formatDate(c.started_at)}
            {!sameDay && ` – ${formatDate(c.ended_at)}`}
          </div>
          <div className="event-facts">
            <span>{formatCount(c.photo_count, "photo")}</span>
            <span>
              {formatTime(c.started_at)} · {formatSpan(c.started_at, c.ended_at)}
            </span>
            {c.anchor_latitude !== undefined && c.anchor_longitude !== undefined ? (
              <span className="mono" title="The event's first GPS fix; distances are measured from here">
                {c.anchor_latitude.toFixed(4)}, {c.anchor_longitude.toFixed(4)}
                {c.max_distance_meters > 0 && ` · within ${formatMeters(c.max_distance_meters)}`}
              </span>
            ) : (
              <span className="faint">No location recorded</span>
            )}
          </div>
        </div>
        <div style={{ display: "flex", gap: 6 }}>
          {c.located_count > 0 && c.located_count < c.photo_count && (
            <span className="badge">
              {c.located_count} of {c.photo_count} located
            </span>
          )}
          {c.confidence === "low" && (
            <span className="badge badge-warn" title="Every photo was dated from its file's modification time">
              Low confidence
            </span>
          )}
          {c.confidence === "mixed" && (
            <span className="badge badge-warn" title="Some photos were dated from file modification times">
              Partly file-dated
            </span>
          )}
        </div>
      </div>
      <div className="filmstrip">
        {c.photos.map((p) => (
          <button key={p.photo_id} onClick={() => onOpen(p.photo_id)} title={p.relative_path}>
            <Thumb photoId={p.photo_id} alt={basename(p.relative_path)} available />
            <time dateTime={p.captured_at}>{formatTime(p.captured_at)}</time>
          </button>
        ))}
      </div>
    </section>
  );
}
