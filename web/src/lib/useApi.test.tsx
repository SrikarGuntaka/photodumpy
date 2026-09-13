import { act, renderHook, waitFor } from "@testing-library/react";
import { useApi } from "./useApi";

/** A promise whose resolution the test controls. */
function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe("useApi", () => {
  it("returns the result of the request", async () => {
    const { result } = renderHook(() => useApi(async () => "hello", []));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.data).toBe("hello");
    expect(result.current.error).toBeUndefined();
  });

  // THE race. A slow request for an old query must never overwrite the result
  // of a newer one, even when it resolves last.
  it("ignores a stale response that resolves after a newer one", async () => {
    const slow = deferred<string>();
    const fast = deferred<string>();
    const pending: Record<string, Promise<string>> = { tri: slow.promise, trip: fast.promise };

    const { result, rerender } = renderHook(
      ({ q }: { q: string }) => useApi(() => pending[q]!, [q]),
      { initialProps: { q: "tri" } },
    );

    // The user keeps typing before "tri" comes back.
    rerender({ q: "trip" });

    // "trip" answers first...
    await act(async () => fast.resolve("results for trip"));
    expect(result.current.data).toBe("results for trip");

    // ...then the slow, stale "tri" response finally lands.
    await act(async () => slow.resolve("results for tri"));

    // It must be discarded. Without the generation check it would win.
    expect(result.current.data).toBe("results for trip");
    expect(result.current.loading).toBe(false);
  });

  it("aborts the previous request when inputs change", async () => {
    const signals: AbortSignal[] = [];
    const { rerender } = renderHook(
      ({ q }: { q: string }) =>
        useApi((signal) => {
          signals.push(signal);
          return new Promise<string>(() => {}); // never settles
        }, [q]),
      { initialProps: { q: "a" } },
    );

    rerender({ q: "b" });
    expect(signals).toHaveLength(2);
    expect(signals[0]!.aborted).toBe(true);
    expect(signals[1]!.aborted).toBe(false);
  });

  it("does not surface its own aborts as errors", async () => {
    const { result, rerender } = renderHook(
      ({ q }: { q: string }) =>
        useApi(
          (signal) =>
            new Promise<string>((_, reject) => {
              signal.addEventListener("abort", () =>
                reject(new DOMException("aborted", "AbortError")),
              );
            }),
          [q],
        ),
      { initialProps: { q: "a" } },
    );

    await act(async () => rerender({ q: "b" }));
    expect(result.current.error).toBeUndefined();
  });

  it("reports real errors", async () => {
    const { result } = renderHook(() =>
      useApi(async () => {
        throw new Error("boom");
      }, []),
    );
    await waitFor(() => expect(result.current.error?.message).toBe("boom"));
    expect(result.current.loading).toBe(false);
  });

  it("keeps previous data visible while a new request loads", async () => {
    const second = deferred<string>();
    const responses = [Promise.resolve("first"), second.promise];
    let call = 0;

    const { result, rerender } = renderHook(
      ({ q }: { q: string }) => useApi(() => responses[call++]!, [q]),
      { initialProps: { q: "a" } },
    );
    await waitFor(() => expect(result.current.data).toBe("first"));

    rerender({ q: "b" });
    // Loading, but the grid is not blanked out while it does.
    expect(result.current.loading).toBe(true);
    expect(result.current.data).toBe("first");

    await act(async () => second.resolve("second"));
    expect(result.current.data).toBe("second");
  });

  it("reload re-runs the request with the same inputs", async () => {
    let n = 0;
    const { result } = renderHook(() => useApi(async () => ++n, []));
    await waitFor(() => expect(result.current.data).toBe(1));

    act(() => result.current.reload());
    await waitFor(() => expect(result.current.data).toBe(2));
  });
});
