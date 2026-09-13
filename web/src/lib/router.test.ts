import { match, navigate } from "./router";

describe("match", () => {
  it("extracts named segments", () => {
    expect(match("/l/:id/:view?", "/l/abc/review")).toEqual({ id: "abc", view: "review" });
  });

  it("allows an optional trailing segment to be absent", () => {
    expect(match("/l/:id/:view?", "/l/abc")).toEqual({ id: "abc" });
  });

  it("rejects extra segments and literal mismatches", () => {
    expect(match("/l/:id/:view?", "/l/abc/review/extra")).toBeNull();
    expect(match("/l/:id", "/x/abc")).toBeNull();
    expect(match("/l/:id", "/l")).toBeNull();
  });

  it("matches the root", () => {
    expect(match("/", "/")).toEqual({});
  });

  it("treats a malformed escape as no match rather than throwing", () => {
    expect(match("/l/:id", "/l/%E0%A4%A")).toBeNull();
  });
});

describe("navigate and history state", () => {
  beforeEach(() => window.history.replaceState(null, "", "/"));

  it("attaches state on push", () => {
    navigate("/l/a?photo=1", { state: { drawer: true } });
    expect(window.history.state).toEqual({ drawer: true });
  });

  // Regression: a replace that wiped the drawer's marker changed what the back
  // button did, and one that ADDED a marker could send back() out of the app.
  it("preserves existing state on replace when none is given", () => {
    navigate("/l/a?photo=1", { state: { drawer: true } });
    navigate("/l/a?photo=2", { replace: true });
    expect(window.history.state).toEqual({ drawer: true });
    expect(window.location.search).toBe("?photo=2");
  });

  it("does not invent state on a replace over a plain entry", () => {
    window.history.replaceState(null, "", "/l/a?photo=1"); // e.g. a pasted link
    navigate("/l/a?photo=2", { replace: true });
    expect(window.history.state).toBeNull();
  });
});
