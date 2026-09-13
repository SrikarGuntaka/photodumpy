import type {
  ApiErrorBody,
  ClusterList,
  DuplicateList,
  JobsResponse,
  Library,
  LibraryDetail,
  PhotoDetail,
  PhotoSearchParams,
  PhotoSearchResult,
  SimilarList,
} from "./types";

/**
 * An error the API reported, carrying its machine-readable code.
 *
 * The API's message is shown to the user as-is. Its errors are written to be
 * read -- "path is outside the photo root", "unknown flag" -- and paraphrasing
 * them in the UI would only lose information.
 */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

/**
 * Builds a query string from a params object, omitting unset values.
 *
 * Omission matters: the API treats an absent has_gps as "no filter" and
 * has_gps=false as "only photos WITHOUT GPS". Serialising undefined as the
 * string "undefined", or false-y values as empty strings, would silently turn
 * one request into the other. So undefined, null and "" are dropped, and
 * false is sent as the literal "false".
 */
export function toQuery(params: object): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === "") continue;
    search.set(key, String(value));
  }
  const s = search.toString();
  return s ? `?${s}` : "";
}

/**
 * Only UUID-shaped ids are interpolated into a URL path.
 *
 * The ids come from API responses and the address bar, and the address bar is
 * user-editable. An id like "../workers" would otherwise quietly turn
 * /api/libraries/{id} into a request for a different endpoint. The server
 * rejects that too; checking here means a bad link fails with a clear message
 * instead of an unrelated response being rendered as if it were a library.
 */
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function assertId(id: string): string {
  if (!UUID.test(id)) {
    throw new ApiError(400, "invalid_id", `"${id}" is not a valid id`);
  }
  return id;
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: { Accept: "application/json", ...(init.body ? { "Content-Type": "application/json" } : {}) },
  });

  // Parse defensively. A proxy error page, or the UI's own index.html served
  // for a mistyped route, is HTML -- and JSON.parse on it throws a SyntaxError
  // that says nothing about what actually went wrong.
  const text = await res.text();
  let body: unknown;
  try {
    body = text ? JSON.parse(text) : undefined;
  } catch {
    throw new ApiError(res.status, "bad_response",
      `The server returned a non-JSON response (HTTP ${res.status}).`);
  }

  if (!res.ok) {
    const err = (body as ApiErrorBody | undefined)?.error;
    throw new ApiError(res.status, err?.code ?? "http_error",
      err?.message ?? `Request failed (HTTP ${res.status}).`);
  }
  return body as T;
}

export const api = {
  listLibraries: (init?: RequestInit) =>
    request<{ libraries: Library[] }>("/api/libraries", init),

  getLibrary: (id: string, init?: RequestInit) =>
    request<LibraryDetail>(`/api/libraries/${assertId(id)}`, init),

  createLibrary: (path: string, name?: string) =>
    request<Library>("/api/libraries", {
      method: "POST",
      body: JSON.stringify(name ? { path, name } : { path }),
    }),

  scanLibrary: (id: string) =>
    request<{ status: string }>(`/api/libraries/${assertId(id)}/scan`, {
      method: "POST",
      body: "{}",
    }),

  processLibrary: (id: string) =>
    request<{ enqueued: number }>(`/api/libraries/${assertId(id)}/process`, {
      method: "POST",
      body: "{}",
    }),

  getJobs: (id: string, init?: RequestInit) =>
    request<JobsResponse>(`/api/libraries/${assertId(id)}/jobs`, init),

  searchPhotos: (id: string, params: PhotoSearchParams, init?: RequestInit) =>
    request<PhotoSearchResult>(`/api/libraries/${assertId(id)}/photos/search${toQuery(params)}`, init),

  getPhoto: (photoId: string, init?: RequestInit) =>
    request<PhotoDetail>(`/api/photos/${assertId(photoId)}`, init),

  listDuplicates: (id: string, params: { limit?: number; offset?: number }, init?: RequestInit) =>
    request<DuplicateList>(`/api/libraries/${assertId(id)}/duplicates${toQuery(params)}`, init),

  listSimilar: (id: string, params: { limit?: number; offset?: number }, init?: RequestInit) =>
    request<SimilarList>(`/api/libraries/${assertId(id)}/similar${toQuery(params)}`, init),

  listClusters: (id: string, params: { limit?: number; offset?: number }, init?: RequestInit) =>
    request<ClusterList>(`/api/libraries/${assertId(id)}/clusters${toQuery(params)}`, init),
};

export const thumbnailUrl = (photoId: string) => `/api/photos/${assertId(photoId)}/thumbnail`;
export const originalUrl = (photoId: string) => `/api/photos/${assertId(photoId)}/original`;
