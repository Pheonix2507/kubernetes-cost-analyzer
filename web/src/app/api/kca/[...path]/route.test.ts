import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";
import { GET } from "./route";

/**
 * The security boundary of the whole dashboard, tested.
 *
 * The API key must never reach the browser, so every request goes through this server-side proxy.
 * The Phase 8 verification was static -- grepping the client bundle for the key -- which proves the
 * key is not in the JavaScript but says nothing about whether the proxy can be talked into
 * forwarding somewhere it should not, or into echoing the key back.
 *
 * These run in Node with no browser, because the handler is a plain function of a Web Request. That
 * is the payoff for it being written that way rather than as middleware.
 */

const KEY = "test-key-not-a-real-credential";

/** call invokes the handler the way Next.js does, with params as a promise. */
function call(pathSegments: string[], search = "") {
  const url = `http://localhost:3100/api/kca/${pathSegments.join("/")}${search}`;
  return GET(new NextRequest(url), { params: Promise.resolve({ path: pathSegments }) });
}

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  process.env.KCA_API_KEY = KEY;
  process.env.KCA_API_URL = "http://kca-api:8080";
  fetchMock = vi.fn().mockResolvedValue(
    new Response(JSON.stringify({ items: [] }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    }),
  );
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  vi.unstubAllGlobals();
  delete process.env.KCA_API_KEY;
});

describe("the allow-list", () => {
  it("forwards a permitted path", async () => {
    const res = await call(["costs", "summary"], "?group_by=namespace");
    expect(res.status).toBe(200);
    expect(fetchMock).toHaveBeenCalledOnce();
  });

  it("refuses a path it does not know, WITHOUT calling upstream", async () => {
    // The second assertion is the important one. A proxy that forwards first and filters the
    // response has already leaked the request, and with it the credential, to whatever the caller
    // named.
    const res = await call(["admin", "shutdown"]);
    expect(res.status).toBe(404);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("answers 404 and not 403, so it reveals nothing about what exists", async () => {
    // A 403 confirms the path is real and merely forbidden, which tells a prober exactly where to
    // aim next. From the browser's side this proxy genuinely has no such route, so 404 is both
    // truthful and uninformative -- the combination you want.
    const res = await call(["v1", "secrets"]);
    expect(res.status).toBe(404);
    const body = await res.json();
    expect(JSON.stringify(body)).not.toContain("forbidden");
  });

  it("cannot be walked out of its allow-list with traversal segments", async () => {
    // `/api/v1/costs/summary/../../../healthz` string-prefixes an allowed path, so a naive
    // startsWith check would pass it through. The allow-list requires an exact match or an exact
    // match followed by `?`, which is what closes this.
    for (const attempt of [
      ["costs", "summary", "..", "..", "admin"],
      ["costs", "summary", "extra"],
      ["costs", "summaryX"],
    ]) {
      const res = await call(attempt);
      expect(res.status, `path ${attempt.join("/")} should not be forwarded`).toBe(404);
    }
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe("the credential", () => {
  it("is attached to the UPSTREAM request", async () => {
    await call(["costs", "summary"]);
    const [, init] = fetchMock.mock.calls[0]!;
    expect((init.headers as Record<string, string>).Authorization).toBe(`Bearer ${KEY}`);
  });

  it("never appears in the response returned to the browser", async () => {
    // The whole point, asserted directly: headers and body both.
    const res = await call(["costs", "summary"]);
    expect(JSON.stringify([...res.headers.entries()])).not.toContain(KEY);
    expect(await res.text()).not.toContain(KEY);
  });

  it("is omitted rather than sent as the string 'undefined' when unset", async () => {
    // A missing key must produce NO Authorization header. Sending `Bearer undefined` would be a
    // request that looks authenticated, fails with 401, and sends someone hunting for a key-rotation
    // bug instead of a missing environment variable.
    delete process.env.KCA_API_KEY;
    await call(["costs", "summary"]);
    const [, init] = fetchMock.mock.calls[0]!;
    expect((init.headers as Record<string, string>).Authorization).toBeUndefined();
  });

  it("is not forwarded FROM the browser even if the caller supplies one", async () => {
    // A client-supplied Authorization header must not reach the API, or the proxy becomes a way to
    // present arbitrary credentials to it.
    const url = "http://localhost:3100/api/kca/costs/summary";
    const req = new NextRequest(url, { headers: { Authorization: "Bearer attacker-supplied" } });
    await GET(req, { params: Promise.resolve({ path: ["costs", "summary"] }) });
    const [, init] = fetchMock.mock.calls[0]!;
    expect((init.headers as Record<string, string>).Authorization).toBe(`Bearer ${KEY}`);
  });
});

describe("the query string", () => {
  it("is passed through, since filters live there", async () => {
    await call(["costs", "summary"], "?group_by=team&cluster=kca-dev");
    const [target] = fetchMock.mock.calls[0]!;
    expect(String(target)).toContain("group_by=team");
    expect(String(target)).toContain("cluster=kca-dev");
  });
});

describe("methods other than GET", () => {
  it("are not exported, so Next.js answers 405 without a rejection branch", async () => {
    // Asserted against the MODULE rather than by making a request, because the absence of an export
    // is the mechanism. A future refactor adding a POST handler for convenience would fail here and
    // have to argue for itself -- this API is read-only.
    const mod = await import("./route");
    for (const method of ["POST", "PUT", "PATCH", "DELETE"]) {
      expect(mod, `${method} must not be exported from a read-only proxy`).not.toHaveProperty(method);
    }
  });
});
