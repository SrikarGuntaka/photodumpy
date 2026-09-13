import { useEffect, useRef } from "react";

/**
 * Calls `tick` every `intervalMs` while `enabled`, and not while the tab is
 * hidden.
 *
 * Pausing when hidden is not only politeness. A forgotten background tab
 * polling a job queue every second is a steady trickle of database queries for
 * a page nobody is looking at. The moment the tab becomes visible again, one
 * tick fires immediately, so returning to the page shows current state rather
 * than state as of when it was hidden.
 *
 * Ticks never overlap: the next is scheduled only after the previous one
 * settles. setInterval would fire regardless, and against a slow response it
 * stacks up concurrent requests that all race to update the same state.
 */
export function usePolling(
  tick: () => Promise<unknown> | void,
  intervalMs: number,
  enabled: boolean,
) {
  const tickRef = useRef(tick);
  tickRef.current = tick;

  useEffect(() => {
    if (!enabled) return;

    let timer: ReturnType<typeof setTimeout> | undefined;
    let stopped = false;

    const run = async () => {
      timer = undefined;
      if (stopped || document.hidden) return;
      try {
        await tickRef.current();
      } catch {
        // A failed poll is retried on the next tick; views show their own
        // error state from their data hooks.
      }
      if (!stopped && !document.hidden) timer = setTimeout(run, intervalMs);
    };

    const onVisibility = () => {
      if (document.hidden) {
        if (timer) clearTimeout(timer);
        timer = undefined;
      } else if (!timer) {
        void run();
      }
    };

    timer = setTimeout(run, intervalMs);
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      stopped = true;
      if (timer) clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [intervalMs, enabled]);
}
