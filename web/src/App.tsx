import { useCallback } from "react";
import { api } from "./api/client";
import { Link } from "./components/Link";
import { PhotoDrawer } from "./components/PhotoDrawer";
import { match, useLocation, useSearchParams } from "./lib/router";
import { useApi } from "./lib/useApi";
import { LibrariesView } from "./views/LibrariesView";
import { OverviewView } from "./views/OverviewView";
import { PhotosView } from "./views/PhotosView";
import { ReviewView } from "./views/ReviewView";
import { TimelineView } from "./views/TimelineView";

const DRAWER_ENTRY = { drawer: true };

const VIEWS = [
  { key: "", label: "Overview" },
  { key: "review", label: "Review" },
  { key: "photos", label: "Photos" },
  { key: "timeline", label: "Timeline" },
] as const;

export function App() {
  const { path } = useLocation();
  const route = match("/l/:id/:view?", path);

  return (
    <>
      <header className="app-header">
        <Link to="/" className="brand">
          <img src="/favicon.svg" alt="" />
          Photo Organizer
        </Link>
        {route && (
          <nav className="nav" aria-label="Library">
            {VIEWS.map((v) => {
              const to = `/l/${route.id}${v.key ? `/${v.key}` : ""}`;
              const current = (route.view ?? "") === v.key;
              return (
                <Link key={v.key} to={to} aria-current={current ? "page" : undefined}>
                  {v.label}
                </Link>
              );
            })}
          </nav>
        )}
      </header>
      <main>{route ? <LibraryShell id={route.id!} view={route.view ?? ""} /> : path === "/" ? <LibrariesView /> : <NotFound />}</main>
    </>
  );
}

/**
 * Loads a library once and hands its summary to whichever view is showing.
 *
 * The summary is fetched here rather than in each view so switching tabs does
 * not re-request it, and so the photo detail drawer can open over any view --
 * it is driven by ?photo= in the URL, which makes an open photo bookmarkable
 * and lets the back button close it.
 */
function LibraryShell({ id, view }: { id: string; view: string }) {
  const detail = useApi((signal) => api.getLibrary(id, { signal }), [id]);
  const [params, setParams] = useSearchParams();
  const photoId = params.get("photo");

  // Opening a photo PUSHES a history entry, marked as the drawer's own, so the
  // back button closes it. Moving between related photos inside the drawer
  // replaces, so back still closes the drawer rather than stepping through
  // every sibling that was glanced at.
  const openPhoto = useCallback(
    // Only a PUSH attaches the marker. Moving within an already-open drawer
    // replaces and passes no state, so the entry keeps whatever it had: a
    // drawer opened from a pasted link must not acquire a marker saying the
    // drawer pushed it, or closing would go back() out of the app.
    (pid: string) => setParams({ photo: pid }, photoId ? {} : { push: true, state: DRAWER_ENTRY }),
    [setParams, photoId],
  );

  // Closing goes BACK only when the current entry is one the drawer pushed.
  //
  // The marker is what makes that safe. A drawer opened from a pasted or
  // bookmarked link has no entry of its own beneath it -- the previous history
  // entry may be a different website entirely -- and calling back() there
  // would navigate the user out of the app. Checking for a null history state,
  // the first version of this, could not tell the two cases apart.
  const closePhoto = useCallback(() => {
    if ((window.history.state as { drawer?: boolean } | null)?.drawer) window.history.back();
    else setParams({ photo: undefined });
  }, [setParams]);

  if (detail.error) {
    return (
      <div className="card empty">
        <h3>Could not open this library</h3>
        <p>{detail.error.message}</p>
        <p style={{ marginTop: 12 }}>
          <Link to="/">Back to libraries</Link>
        </p>
      </div>
    );
  }
  if (!detail.data) return <p className="muted">Loading…</p>;

  const props = { libraryId: id, detail: detail.data, onOpenPhoto: openPhoto };

  return (
    <>
      {view === "" && <OverviewView libraryId={id} detail={detail.data} onChanged={detail.reload} />}
      {view === "review" && <ReviewView {...props} />}
      {view === "photos" && <PhotosView {...props} />}
      {view === "timeline" && <TimelineView {...props} />}
      {!["", "review", "photos", "timeline"].includes(view) && <NotFound />}
      {photoId && <PhotoDrawer photoId={photoId} onClose={closePhoto} onOpen={openPhoto} />}
    </>
  );
}

function NotFound() {
  return (
    <div className="card empty">
      <h3>Page not found</h3>
      <p>
        <Link to="/">Back to libraries</Link>
      </p>
    </div>
  );
}
