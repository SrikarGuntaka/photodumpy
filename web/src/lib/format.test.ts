import {
  DASH,
  basename,
  formatBytes,
  formatDate,
  formatDateTime,
  formatDims,
  formatFlag,
  formatMeters,
  formatScore,
  formatSpan,
} from "./format";

describe("absent is never zero", () => {
  // The single most important formatting rule. An unmeasured photo rendered as
  // "0.00" reads as the worst photo in the library.
  it("renders a missing score as a dash", () => {
    expect(formatScore(undefined)).toBe(DASH);
    expect(formatScore(0)).toBe("0.00");
  });

  it("renders missing dimensions, bytes and distance as a dash", () => {
    expect(formatDims(undefined, 480)).toBe(DASH);
    expect(formatBytes(undefined)).toBe(DASH);
    expect(formatMeters(undefined)).toBe(DASH);
    expect(formatMeters(0)).toBe("0 m");
  });

  it("renders missing and malformed dates as a dash", () => {
    expect(formatDate(undefined)).toBe(DASH);
    expect(formatDate("")).toBe(DASH);
    expect(formatDate("not a date")).toBe(DASH);
  });
});

describe("formatBytes", () => {
  it.each([
    [0, "0 B"],
    [1023, "1023 B"],
    [1024, "1.0 KB"],
    [61_571, "60.1 KB"],
    [5 * 1024 * 1024, "5.0 MB"],
    [312 * 1024 * 1024, "312 MB"],
    [4.2 * 1024 ** 3, "4.2 GB"],
  ])("%d -> %s", (n, want) => {
    expect(formatBytes(n)).toBe(want);
  });

  it("refuses nonsense rather than printing it", () => {
    expect(formatBytes(-1)).toBe(DASH);
    expect(formatBytes(Number.NaN)).toBe(DASH);
  });
});

describe("dates are rendered in UTC", () => {
  // EXIF times were read as UTC upstream. Rendering in the viewer's zone would
  // move a 23:30 photo to the next day in any zone east of UTC.
  it("does not shift a late-evening photo onto the next day", () => {
    expect(formatDate("2026-03-14T23:30:00Z")).toBe("14 Mar 2026");
    expect(formatDateTime("2026-03-14T23:30:00Z")).toBe("14 Mar 2026, 23:30");
  });
});

describe("formatSpan", () => {
  it.each([
    ["2026-03-14T09:00:00Z", "2026-03-14T09:00:20Z", "under a minute"],
    ["2026-03-14T09:00:00Z", "2026-03-14T09:47:00Z", "47 min"],
    ["2026-03-14T09:00:00Z", "2026-03-14T13:00:00Z", "4 h"],
    ["2026-03-14T22:19:00Z", "2026-03-15T03:01:00Z", "4 h 42 min"],
    ["2026-03-14T09:00:00Z", "2026-03-16T12:00:00Z", "2 d 3 h"],
  ])("%s to %s -> %s", (a, b, want) => {
    expect(formatSpan(a, b)).toBe(want);
  });

  it("does not render a negative span", () => {
    expect(formatSpan("2026-03-14T10:00:00Z", "2026-03-14T09:00:00Z")).toBe(DASH);
  });
});

describe("small helpers", () => {
  it("keeps the hedge in flag names", () => {
    expect(formatFlag("possibly_underexposed")).toBe("Possibly underexposed");
  });

  it("takes the last path segment", () => {
    expect(basename("trip/day1/scene02.jpg")).toBe("scene02.jpg");
    expect(basename("scene02.jpg")).toBe("scene02.jpg");
  });
});
