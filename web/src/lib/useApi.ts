import { useCallback, useEffect, useRef, useState } from "react";

export interface ApiState<T> {
  data: T | undefined;
  error: Error | undefined;
  loading: boolean;
  /** Re-runs the request with the same inputs. */
  reload: () => void;
}

/**
 * Runs an async request and tracks its result, keyed on `deps`.
 *
 * THE PROBLEM THIS SOLVES is a race every search UI has. Type "tri", then
 * "trip": two requests fly. If "tri" is slower -- it matches more rows -- its
 * response lands AFTER "trip"'s and overwrites it, and the grid shows results
 * for a query the user is no longer looking at. Nothing errors; it is simply
 * wrong.
 *
 * Two defences, because each covers a gap in the other:
 *
 *   1. Every request gets an AbortController, aborted as soon as the inputs
 *      change. That stops the stale work, but abort is advisory: a response
 *      that had already arrived can still be resolving.
 *   2. So each request also captures a generation number, and its result is
 *      only committed if no newer request has started since. That check is
 *      what actually makes the race impossible.
 *
 * Previous data is kept while a new request loads, so changing a filter dims
 * the grid instead of blanking it and making the page jump.
 */
export function useApi<T>(
  fetcher: (signal: AbortSignal) => Promise<T>,
  deps: readonly unknown[],
): ApiState<T> {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<Error>();
  const [loading, setLoading] = useState(true);
  const [nonce, setNonce] = useState(0);
  const generation = useRef(0);

  useEffect(() => {
    const gen = ++generation.current;
    const controller = new AbortController();
    setLoading(true);

    fetcher(controller.signal).then(
      (result) => {
        if (gen !== generation.current) return; // superseded
        setData(result);
        setError(undefined);
        setLoading(false);
      },
      (err: unknown) => {
        if (gen !== generation.current) return; // superseded
        // An abort is this hook cancelling its own stale request, not a
        // failure the user needs to see.
        if (err instanceof DOMException && err.name === "AbortError") return;
        setError(err instanceof Error ? err : new Error(String(err)));
        setLoading(false);
      },
    );

    return () => controller.abort();
    // The caller's deps ARE the dependency list. fetcher is a new closure on
    // every render and would re-fire the request on each one if listed.
  }, [...deps, nonce]);

  const reload = useCallback(() => setNonce((n) => n + 1), []);
  return { data, error, loading, reload };
}
