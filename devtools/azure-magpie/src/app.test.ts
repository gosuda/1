import { describe, expect, test } from "bun:test";
import { resolveRoute } from "./app";

const ID = `I${"3f9a1c2e".repeat(5)}`;

describe("resolveRoute", () => {
  test("maps the real paths of the UI to their views", () => {
    expect(resolveRoute("/")).toEqual({ view: "overview", param: "" });
    expect(resolveRoute("/index.html")).toEqual({ view: "overview", param: "" });
    expect(resolveRoute("/changes")).toEqual({ view: "changes", param: "" });
    expect(resolveRoute("/changes/")).toEqual({ view: "changes", param: "" });
    expect(resolveRoute("/diff")).toEqual({ view: "diff", param: "" });
  });

  test("passes change ids and review numbers to the change view", () => {
    expect(resolveRoute(`/changes/${ID}`)).toEqual({ view: "change", param: ID });
    expect(resolveRoute("/changes/7")).toEqual({ view: "change", param: "7" });
    expect(resolveRoute(`/changes/${ID}/`)).toEqual({ view: "change", param: ID });
  });

  test("decodes percent-encoded params and keeps malformed ones as typed", () => {
    expect(resolveRoute("/changes/%49%31")).toEqual({ view: "change", param: "I1" });
    expect(resolveRoute("/changes/%E0%A4%A")).toEqual({ view: "change", param: "%E0%A4%A" });
  });

  test("routes unknown paths to the not-found view", () => {
    expect(resolveRoute("/workspaces").view).toBe("notfound");
    expect(resolveRoute("/projects/x").view).toBe("notfound");
  });
});
