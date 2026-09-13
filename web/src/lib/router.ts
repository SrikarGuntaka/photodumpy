import { useCallback, useEffect, useState } from "react";

// A small History-API router.
//
// The app has a handful of screens, and their state lives in the URL: the
// library, the view, the filters, and which photo's detail panel is open. That
// makes every screen bookmarkable, makes the back button close the detail panel
// instead of leaving the app, and means a refresh restores exactly what was on
// screen.

export interface Location {
  path: string;
  params: URLSearchParams;
}

function read(): Location {
  return {
    path: window.location.pathname,
    params: new URLSearchParams(window.location.search),
  };
}

const listeners = new Set<() => void>();

/**
 * Changes the URL. A replace PRESERVES the current entry's history state unless
 * a new state is given: replacing is "this same entry, amended", and silently
 * wiping state an earlier push attached -- such as the drawer's marker -- would
 * change what the back button does.
 */
export function navigate(to: string, opts: { replace?: boolean; state?: unknown } = {}) {
  if (to === window.location.pathname + window.location.search) return;
  const state = opts.state !== undefined ? opts.state : opts.replace ? window.history.state : null;
  if (opts.replace) window.history.replaceState(state, "", to);
  else window.history.pushState(state, "", to);
  listeners.forEach((l) => l());
}

export function useLocation(): Location {
  const [loc, setLoc] = useState(read);
  useEffect(() => {
    const update = () => setLoc(read());
    listeners.add(update);
    window.addEventListener("popstate", update);
    return () => {
      listeners.delete(update);
      window.removeEventListener("popstate", update);
    };
  }, []);
  return loc;
}

/**
 * Returns the query params and a setter that merges changes into them.
 *
 * Filter changes REPLACE the history entry rather than pushing one. Typing a
 * search term would otherwise add an entry per keystroke, and the back button
 * would step back through "tri", "tr", "t" instead of leaving the page.
 * Opening a photo passes push, so back closes it.
 */
export function useSearchParams() {
  const { path, params } = useLocation();
  const set = useCallback(
    (changes: Record<string, string | undefined>, opts: { push?: boolean; state?: unknown } = {}) => {
      const next = new URLSearchParams(window.location.search);
      for (const [k, v] of Object.entries(changes)) {
        if (v === undefined || v === "") next.delete(k);
        else next.set(k, v);
      }
      const qs = next.toString();
      navigate(`${window.location.pathname}${qs ? `?${qs}` : ""}`, { replace: !opts.push, state: opts.state });
    },
    [],
  );
  return [params, set, path] as const;
}

/**
 * Matches a path against a pattern like "/l/:id/:view?". Returns the named
 * segments, or null when the path does not match.
 */
export function match(pattern: string, path: string): Record<string, string> | null {
  const p = pattern.split("/").filter(Boolean);
  const s = path.split("/").filter(Boolean);
  if (s.length > p.length) return null;

  const out: Record<string, string> = {};
  for (let i = 0; i < p.length; i++) {
    const seg = p[i]!;
    const val = s[i];
    const optional = seg.endsWith("?");
    const isParam = seg.startsWith(":");
    const name = seg.replace(/^:/, "").replace(/\?$/, "");

    if (val === undefined) {
      if (optional) continue;
      return null;
    }
    if (isParam) {
      try {
        out[name] = decodeURIComponent(val);
      } catch {
        // A malformed escape like "%E0%A4%A" is not a match, not a crash.
        return null;
      }
    } else if (seg !== val) {
      return null;
    }
  }
  return out;
}
