import { ApiError, api, assertId, toQuery } from "./client";

const ID = "8142fe59-1e27-4655-8076-436e8b00d498";

describe("toQuery", () => {
  // has_gps=false means "only photos WITHOUT GPS"; an absent has_gps means no
  // filter. Serialising the two identically would silently swap them.
  it("sends false explicitly and omits undefined", () => {
    expect(toQuery({ has_gps: false })).toBe("?has_gps=false");
    expect(toQuery({ has_gps: undefined })).toBe("");
  });

  it("omits empty strings and null", () => {
    expect(toQuery({ q: "", flag: null, sort: "quality" })).toBe("?sort=quality");
  });

  it("keeps zero, which is a real value", () => {
    expect(toQuery({ offset: 0 })).toBe("?offset=0");
  });

  it("encodes reserved characters rather than passing them through", () => {
    // An & or # in a search term must not split or truncate the query.
    const q = toQuery({ q: "a&b#c d" });
    expect(new URLSearchParams(q.slice(1)).get("q")).toBe("a&b#c d");
  });
});

describe("assertId", () => {
  it("accepts a UUID in either case", () => {
    expect(assertId(ID)).toBe(ID);
    expect(assertId(ID.toUpperCase())).toBe(ID.toUpperCase());
  });

  // Ids come from the address bar, which the user can edit. An id that is not
  // a UUID must never be spliced into an API path.
  it.each(["../workers", "", "abc", `${ID}/../../workers`, "%2e%2e"])("rejects %j", (bad) => {
    expect(() => assertId(bad)).toThrow(ApiError);
  });
});

describe("request error handling", () => {
  afterEach(() => vi.unstubAllGlobals());

  function stubFetch(status: number, body: string) {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(body, { status })));
  }

  it("surfaces the API's own error message and code", async () => {
    stubFetch(400, JSON.stringify({ error: { code: "invalid_request", message: "unknown flag \"x\"" } }));
    await expect(api.listLibraries()).rejects.toMatchObject({
      status: 400,
      code: "invalid_request",
      message: 'unknown flag "x"',
    });
  });

  // A proxy error page or a stray index.html is HTML. The raw JSON.parse
  // SyntaxError would say nothing useful about what went wrong.
  it("explains a non-JSON response instead of throwing a SyntaxError", async () => {
    stubFetch(502, "<html>Bad Gateway</html>");
    const err = await api.listLibraries().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).code).toBe("bad_response");
    expect((err as ApiError).message).toContain("502");
  });

  it("never issues a request for an invalid id", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    // assertId throws synchronously while the request is being built.
    expect(() => api.getLibrary("../workers")).toThrow(ApiError);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
