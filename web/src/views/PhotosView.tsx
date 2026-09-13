import { useEffect, useRef, useState, type ReactNode } from "react";
import { api } from "../api/client";
import type { LibraryDetail, PhotoSearchParams, PhotoSort, PhotoView } from "../api/types";
import { Thumb } from "../components/Thumb";
import { basename, formatBytes, formatCount, formatDate, formatFlag, formatScore } from "../lib/format";
import { useSearchParams } from "../lib/router";
import { useApi } from "../lib/useApi";

const PAGE = 60;

const SORTS: { value: PhotoSort; label: string; defaultOrder: "asc" | "desc" }[] = [
  { value: "path", label: "Path", defaultOrder: "asc" },
  { value: "captured_at", label: "Date taken", defaultOrder: "desc" },
  { value: "quality", label: "Quality (worst first)", defaultOrder: "asc" },
  { value: "file_size", label: "File size", defaultOrder: "desc" },
];

/** Reads a tri-state boolean from the URL: absent, "true" or "false". */
function triState(v: string | null): boolean | undefined {
  return v === "true" ? true : v === "false" ? false : undefined;
}

export function PhotosView({ libraryId, detail, onOpenPhoto }: {
  libraryId: string;
  detail: LibraryDetail;
  onOpenPhoto: (id: string) => void;
}) {
  const [params, setParams] = useSearchParams();

  // The URL is the single source of truth for filters. Local state holds only
  // the text being typed, so the search box stays responsive while the URL --
  // and therefore the request -- updates after a pause.
  const urlQuery = params.get("q") ?? "";
  const [draft, setDraft] = useState(urlQuery);
  useEffect(() => setDraft(urlQuery), [urlQuery]);
  useEffect(() => {
    if (draft === urlQuery) return;
    // Debounced. A request per keystroke is wasteful, and useApi discards any
    // stale response anyway, so this is about load, not correctness.
    const t = setTimeout(() => setParams({ q: draft || undefined }), 250);
    return () => clearTimeout(t);
  }, [draft, urlQuery, setParams]);

  const sort = (params.get("sort") as PhotoSort | null) ?? "path";
  const sortDef = SORTS.find((s) => s.value === sort) ?? SORTS[0]!;
  const order = (params.get("order") as "asc" | "desc" | null) ?? sortDef.defaultOrder;

  const filters: PhotoSearchParams = {
    q: urlQuery || undefined,
    flag: params.get("flag") ?? undefined,
    has_flags: triState(params.get("has_flags")),
    has_gps: triState(params.get("has_gps")),
    has_duplicates: triState(params.get("has_duplicates")),
    has_similar: triState(params.get("has_similar")),
    sort,
    order,
  };
  const filterKey = JSON.stringify(filters);

  // The first page comes from useApi, keyed on the filters, so it gets the
  // stale-response protection for free.
  const result = useApi(
    (signal) => api.searchPhotos(libraryId, { ...filters, limit: PAGE, offset: 0 }, { signal }),
    [libraryId, filterKey],
  );

  // Further pages are fetched by OFFSET and appended.
  //
  // Not by growing the limit. That was the first version, and the API clamps
  // any limit above 500 back to its default of 100 -- so the ninth "Load more"
  // would have silently shrunk a 480-photo grid to 100.
  //
  // Appended pages are tagged with the filter key they were fetched for. A
  // page that lands after the filters changed carries the old key, is never
  // shown, and cannot be appended to results for a different query.
  const [extra, setExtra] = useState<{ key: string; photos: PhotoView[] }>({ key: "", photos: [] });
  const [loadingMore, setLoadingMore] = useState(false);
  const liveKey = useRef(filterKey);
  liveKey.current = filterKey;

  const firstPage = result.data?.photos ?? [];
  const extraPhotos = extra.key === filterKey ? extra.photos : [];
  // De-duplicated by id: if photos are added while paging, offsets shift and
  // a photo can appear at the end of one page and the start of the next.
  const photos: PhotoView[] = dedupe([...firstPage, ...extraPhotos]);
  const total = result.data?.total ?? 0;

  async function loadMore() {
    const key = filterKey;
    const offset = firstPage.length + extraPhotos.length;
    setLoadingMore(true);
    try {
      const page = await api.searchPhotos(libraryId, { ...filters, limit: PAGE, offset });
      if (liveKey.current !== key) return; // filters changed while loading
      setExtra((prev) => ({
        key,
        photos: [...(prev.key === key ? prev.photos : []), ...page.photos],
      }));
    } finally {
      setLoadingMore(false);
    }
  }
  const flags = Object.entries(detail.quality.by_flag).sort((a, b) => b[1] - a[1]);
  const anyFilter = !!(filters.q || filters.flag || filters.has_flags !== undefined ||
    filters.has_gps !== undefined || filters.has_duplicates !== undefined || filters.has_similar !== undefined);

  function toggle(key: string, value: string) {
    setParams({ [key]: params.get(key) === value ? undefined : value });
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Photos</h1>
          <p>Search, filter and sort everything the pipeline found.</p>
        </div>
      </div>

      <div className="toolbar">
        <label className="sr-only" htmlFor="photo-search">Search paths</label>
        <input
          id="photo-search"
          className="input"
          type="search"
          placeholder="Search file paths…"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          spellCheck={false}
        />
        <label className="sr-only" htmlFor="photo-sort">Sort by</label>
        <select
          id="photo-sort"
          className="select"
          value={sort}
          onChange={(e) => {
            const next = SORTS.find((s) => s.value === e.target.value)!;
            // Changing the sort resets direction to what that sort means by
            // default -- newest first for dates, worst first for quality.
            setParams({ sort: next.value === "path" ? undefined : next.value, order: undefined });
          }}
        >
          {SORTS.map((s) => (
            <option key={s.value} value={s.value}>{s.label}</option>
          ))}
        </select>
        <button
          className="btn"
          onClick={() => setParams({ order: order === "asc" ? "desc" : "asc" })}
          aria-label={`Sort direction: ${order === "asc" ? "ascending" : "descending"}`}
          title="Reverse sort"
        >
          {order === "asc" ? "↑ Asc" : "↓ Desc"}
        </button>
      </div>

      <div className="chips" role="group" aria-label="Filters">
        <Chip on={params.get("has_duplicates") === "true"} onClick={() => toggle("has_duplicates", "true")}>
          Exact duplicates
        </Chip>
        <Chip on={params.get("has_similar") === "true"} onClick={() => toggle("has_similar", "true")}>
          Near-duplicates
        </Chip>
        <Chip on={params.get("has_flags") === "true"} onClick={() => toggle("has_flags", "true")}>
          Any quality flag <span className="count">{detail.quality.flagged}</span>
        </Chip>
        {flags.map(([flag, n]) => (
          <Chip key={flag} on={params.get("flag") === flag} onClick={() => toggle("flag", flag)}>
            {formatFlag(flag)} <span className="count">{n}</span>
          </Chip>
        ))}
        <Chip on={params.get("has_gps") === "true"} onClick={() => toggle("has_gps", "true")}>
          Has GPS
        </Chip>
        <Chip on={params.get("has_gps") === "false"} onClick={() => toggle("has_gps", "false")}>
          No GPS
        </Chip>
        {anyFilter && (
          <button
            className="btn btn-sm"
            onClick={() =>
              setParams({
                q: undefined, flag: undefined, has_flags: undefined, has_gps: undefined,
                has_duplicates: undefined, has_similar: undefined,
              })
            }
          >
            Clear filters
          </button>
        )}
      </div>

      {result.error && (
        <div className="notice notice-danger" role="alert">
          <strong>Search failed.</strong> {result.error.message}
        </div>
      )}

      {result.data && (
        <p className="result-meta" aria-live="polite">
          {formatCount(total, "photo")}
          {anyFilter ? " match" : ""}
          {total > photos.length ? ` · showing ${photos.length}` : ""}
        </p>
      )}

      {result.data && photos.length === 0 ? (
        <div className="card empty">
          <h3>No photos match</h3>
          <p>{anyFilter ? "Try removing a filter." : "This library has no photos yet."}</p>
        </div>
      ) : (
        <div className={`photo-grid ${result.loading && result.data ? "stale" : ""}`}>
          {photos.map((p) => (
            <Tile key={p.id} photo={p} onOpen={() => onOpenPhoto(p.id)} />
          ))}
        </div>
      )}

      {photos.length < total && (
        <div className="load-more">
          <button className="btn" onClick={loadMore} disabled={loadingMore || result.loading}>
            {loadingMore && <span className="spinner" aria-hidden="true" />}
            Load {Math.min(PAGE, total - photos.length)} more
          </button>
        </div>
      )}
    </>
  );
}

function dedupe(list: PhotoView[]): PhotoView[] {
  const seen = new Set<string>();
  return list.filter((p) => (seen.has(p.id) ? false : (seen.add(p.id), true)));
}

function Chip({ on, onClick, children }: { on: boolean; onClick: () => void; children: ReactNode }) {
  return (
    <button type="button" className="chip" aria-pressed={on} onClick={onClick}>
      {children}
    </button>
  );
}

function Tile({ photo: p, onOpen }: { photo: PhotoView; onOpen: () => void }) {
  const name = basename(p.relative_path);
  return (
    <button className="tile" onClick={onOpen} title={p.relative_path}>
      <span style={{ position: "relative", display: "block" }}>
        <span className="tile-badges">
          {p.duplicate_group_id && <span className="badge badge-danger">Duplicate</span>}
          {!p.duplicate_group_id && p.similar_group_id && <span className="badge badge-accent">Similar</span>}
          {p.quality_flags.length > 0 && (
            <span className="badge badge-warn" title={p.quality_flags.map(formatFlag).join(", ")}>
              {p.quality_flags.length === 1 ? formatFlag(p.quality_flags[0]!) : `${p.quality_flags.length} flags`}
            </span>
          )}
        </span>
        <Thumb photoId={p.id} alt={name} available={p.thumbnail_width !== undefined} />
      </span>
      <span className="tile-name">{name}</span>
      <span className="tile-meta">
        <span>{p.captured_at ? formatDate(p.captured_at) : "No date"}</span>
        <span>{formatBytes(p.file_size_bytes)}</span>
        <span title="Overall technical quality">Q {formatScore(p.quality_score)}</span>
      </span>
    </button>
  );
}
