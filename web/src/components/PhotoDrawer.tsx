import { useEffect, useRef, type ReactNode } from "react";
import { api, originalUrl } from "../api/client";
import type { RelatedPhoto } from "../api/types";
import {
  DASH,
  basename,
  formatBytes,
  formatDateTime,
  formatDims,
  formatFlag,
  formatMeters,
  formatScore,
} from "../lib/format";
import { useApi } from "../lib/useApi";
import { Thumb } from "./Thumb";

interface Props {
  photoId: string;
  onClose: () => void;
  onOpen: (photoId: string) => void;
}

/**
 * Everything known about one photo, in a side panel.
 *
 * A dialog in the accessibility sense: focus moves into it on open, Escape
 * closes it, and focus returns to whatever opened it on close -- so a keyboard
 * user reviewing a grid lands back on the tile they were on, not at the top of
 * the page.
 */
export function PhotoDrawer({ photoId, onClose, onOpen }: Props) {
  const { data, error, loading } = useApi((signal) => api.getPhoto(photoId, { signal }), [photoId]);
  const closeRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    const returnTo = document.activeElement as HTMLElement | null;
    closeRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    // The page behind must not scroll while the panel is open.
    const overflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = overflow;
      returnTo?.focus?.();
    };
    // Registered once per open panel; onClose identity changes per render.
  }, []);

  const p = data?.photo;
  // Only show relations from the CURRENT response. useApi keeps previous data
  // while a new photo loads, which is right for a grid but wrong here: the
  // header would name the new photo while the siblings belong to the old one.
  const current = p?.id === photoId ? data : undefined;

  return (
    <>
      <div className="scrim" onClick={onClose} />
      <aside className="drawer" role="dialog" aria-modal="true" aria-labelledby="drawer-title">
        <div className="drawer-head">
          <h2 id="drawer-title">{current ? current.photo.relative_path : "Loading…"}</h2>
          <div style={{ display: "flex", gap: 8 }}>
            {current && (
              <a className="btn btn-sm" href={originalUrl(photoId)} target="_blank" rel="noopener noreferrer">
                Open original
              </a>
            )}
            <button ref={closeRef} className="btn btn-sm" onClick={onClose} aria-label="Close details">
              Close
            </button>
          </div>
        </div>

        <div className="drawer-body">
          {error && (
            <div className="notice notice-danger">
              <strong>Could not load this photo.</strong> {error.message}
            </div>
          )}
          {loading && !current && !error && <p className="muted">Loading…</p>}

          {current && (
            <>
              <div className="drawer-image">
                {current.photo.thumbnail_width !== undefined ? (
                  <img src={originalUrl(photoId)} alt={current.photo.original_filename} />
                ) : (
                  <div className="thumb-missing" style={{ position: "relative", height: "100%" }}>
                    No preview available for this file.
                  </div>
                )}
              </div>

              <Section title="Details">
                <dl className="facts">
                  <Fact label="Size">
                    {formatBytes(current.photo.file_size_bytes)} · {current.photo.detected_format.toUpperCase()}
                  </Fact>
                  <Fact label="Dimensions">{formatDims(current.photo.width, current.photo.height)}</Fact>
                  <Fact label="Captured">
                    {current.photo.captured_at ? (
                      <>
                        {formatDateTime(current.photo.captured_at)}{" "}
                        {current.photo.captured_at_source === "filesystem" ? (
                          <span className="badge badge-warn" title="No camera timestamp; this is the file's modification time">
                            file date
                          </span>
                        ) : (
                          <span className="badge">camera</span>
                        )}
                      </>
                    ) : (
                      <span className="faint">Unknown — no camera date and no usable file date</span>
                    )}
                  </Fact>
                  <Fact label="Location">
                    {current.photo.latitude !== undefined && current.photo.longitude !== undefined ? (
                      <span className="mono">
                        {current.photo.latitude.toFixed(5)}, {current.photo.longitude.toFixed(5)}
                      </span>
                    ) : (
                      <span className="faint">Not recorded</span>
                    )}
                  </Fact>
                  {(current.photo.camera_make || current.photo.camera_model) && (
                    <Fact label="Camera">
                      {[current.photo.camera_make, current.photo.camera_model].filter(Boolean).join(" ")}
                    </Fact>
                  )}
                  {current.photo.last_error && (
                    <Fact label="Last error">
                      <span style={{ color: "var(--danger)" }}>{current.photo.last_error}</span>
                    </Fact>
                  )}
                </dl>
              </Section>

              <Section
                title="Technical quality"
                hint="Measurements of the pixels, not judgements of the photograph. A shallow-focus portrait measures as blurry and may be the best shot in the library."
              >
                <div className="scores">
                  <Score label="Sharpness" value={current.photo.sharpness} />
                  <Score label="Exposure" value={current.photo.exposure} />
                  <Score label="Contrast" value={current.photo.contrast} />
                  <Score label="Resolution" value={current.photo.resolution} />
                  <Score label="Overall" value={current.photo.quality_score} />
                </div>
                {current.photo.quality_flags.length > 0 && (
                  <div className="chips" style={{ marginTop: 12, marginBottom: 0 }}>
                    {current.photo.quality_flags.map((f) => (
                      <span key={f} className="badge badge-warn">
                        {formatFlag(f)}
                      </span>
                    ))}
                  </div>
                )}
              </Section>

              <Related
                title="Exact duplicates"
                hint="Byte-for-byte identical files."
                items={current.relations.duplicates}
                onOpen={onOpen}
              />
              <Related
                title="Near-duplicates"
                hint="Visually similar. Distance is bits differing out of 64."
                items={current.relations.similar}
                caption={(r) => (r.distance !== undefined ? `distance ${r.distance}` : "")}
                onOpen={onOpen}
              />
              <Related
                title="Same event"
                hint="Taken around the same time and place."
                items={current.relations.cluster}
                caption={(r) => (r.distance !== undefined ? formatMeters(r.distance) : "")}
                onOpen={onOpen}
              />

              <Section title="Fingerprints">
                <dl className="facts">
                  <Fact label="SHA-256">
                    <span className="mono">{current.photo.sha256 ?? DASH}</span>
                  </Fact>
                  <Fact label="Perceptual hash">
                    <span className="mono">{current.photo.phash ?? DASH}</span>
                  </Fact>
                </dl>
              </Section>
            </>
          )}
        </div>
      </aside>
    </>
  );
}

function Section({ title, hint, children }: { title: string; hint?: string; children: ReactNode }) {
  return (
    <section style={{ marginBottom: 24 }}>
      <h2 style={{ margin: "0 0 4px" }}>{title}</h2>
      {hint && (
        <p className="faint" style={{ margin: "0 0 10px", fontSize: 12.5 }}>
          {hint}
        </p>
      )}
      {!hint && <div style={{ height: 6 }} />}
      {children}
    </section>
  );
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt>{label}</dt>
      <dd>{children}</dd>
    </>
  );
}

function Score({ label, value }: { label: string; value: number | undefined }) {
  return (
    <div className="score-row">
      <span className="muted">{label}</span>
      <div className="bar" aria-hidden="true">
        {value !== undefined && <span className="running" style={{ width: `${value * 100}%` }} />}
      </div>
      <span className="value">{formatScore(value)}</span>
    </div>
  );
}

function Related({
  title,
  hint,
  items,
  caption,
  onOpen,
}: {
  title: string;
  hint: string;
  items: RelatedPhoto[];
  caption?: (r: RelatedPhoto) => string;
  onOpen: (id: string) => void;
}) {
  // A group of one is only the photo itself.
  if (items.length < 2) return null;
  return (
    <Section title={`${title} (${items.length})`} hint={hint}>
      <div className="related">
        {items.map((r) => (
          <button
            key={r.photo_id}
            className={r.is_self ? "self" : ""}
            onClick={() => !r.is_self && onOpen(r.photo_id)}
            aria-current={r.is_self ? "true" : undefined}
            title={r.relative_path}
          >
            <span style={{ position: "relative", display: "block" }}>
              {(r.suggested_keep || r.is_self) && (
                <span className="tile-badges">
                  {r.suggested_keep && <span className="badge badge-keep">Keep</span>}
                  {r.is_self && <span className="badge badge-accent">This photo</span>}
                </span>
              )}
              {/* Related photos carry no thumbnail flag; the image's own
                  onError handles the ones that have none. */}
              <Thumb photoId={r.photo_id} alt={basename(r.relative_path)} available />
            </span>
            <span className="caption">{basename(r.relative_path)}</span>
            {caption && caption(r) && <span className="caption faint">{caption(r)}</span>}
          </button>
        ))}
      </div>
    </Section>
  );
}
