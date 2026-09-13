// Display formatting.
//
// One rule runs through all of it: an absent value renders as a dash, never as
// zero. A photo with no quality score has not been measured; showing 0.00 would
// present it as the worst photo in the library.

export const DASH = "—";

export function formatBytes(n: number | undefined): string {
  if (n === undefined || !Number.isFinite(n) || n < 0) return DASH;
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n;
  let i = -1;
  do {
    v /= 1024;
    i++;
  } while (v >= 1024 && i < units.length - 1);
  // One decimal below 100, none above: "4.2 GB" but "312 MB".
  return `${v < 100 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}

export function formatScore(s: number | undefined): string {
  return s === undefined ? DASH : s.toFixed(2);
}

export function formatDims(w: number | undefined, h: number | undefined): string {
  return w === undefined || h === undefined ? DASH : `${w}×${h}`;
}

export function formatMeters(m: number | undefined): string {
  if (m === undefined) return DASH;
  return m < 1000 ? `${Math.round(m)} m` : `${(m / 1000).toFixed(1)} km`;
}

export function formatCount(n: number, noun: string, plural = `${noun}s`): string {
  return `${n.toLocaleString("en-US")} ${n === 1 ? noun : plural}`;
}

// Timestamps from the API are UTC instants. EXIF wall-clock times were read as
// UTC upstream (the format records no zone), so they are rendered in UTC here
// too. Converting to the viewer's local zone would shift a photo taken at 23:30
// onto the next day -- and the date is exactly what a timeline groups by.
const dateFmt = new Intl.DateTimeFormat("en-GB", {
  day: "numeric",
  month: "short",
  year: "numeric",
  timeZone: "UTC",
});
const timeFmt = new Intl.DateTimeFormat("en-GB", {
  hour: "2-digit",
  minute: "2-digit",
  timeZone: "UTC",
});

export function formatDate(iso: string | undefined): string {
  if (!iso) return DASH;
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? DASH : dateFmt.format(d);
}

export function formatDateTime(iso: string | undefined): string {
  if (!iso) return DASH;
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? DASH : `${dateFmt.format(d)}, ${timeFmt.format(d)}`;
}

export function formatTime(iso: string | undefined): string {
  if (!iso) return DASH;
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? DASH : timeFmt.format(d);
}

export function formatSpan(startIso: string, endIso: string): string {
  const ms = new Date(endIso).getTime() - new Date(startIso).getTime();
  if (!Number.isFinite(ms) || ms < 0) return DASH;
  const min = Math.round(ms / 60000);
  if (min < 1) return "under a minute";
  if (min < 60) return `${min} min`;
  const h = Math.floor(min / 60);
  const rem = min % 60;
  if (h < 24) return rem ? `${h} h ${rem} min` : `${h} h`;
  const d = Math.floor(h / 24);
  return `${d} d ${h % 24} h`;
}

// "possibly_underexposed" -> "Possibly underexposed". The hedge in the name is
// kept on purpose: the measurement is certain, the judgement is not.
export function formatFlag(flag: string): string {
  const s = flag.replace(/_/g, " ");
  return s.charAt(0).toUpperCase() + s.slice(1);
}

export function basename(path: string): string {
  const i = path.lastIndexOf("/");
  return i === -1 ? path : path.slice(i + 1);
}
