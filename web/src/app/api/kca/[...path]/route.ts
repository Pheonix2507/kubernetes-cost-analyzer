import { NextRequest } from "next/server";

/**
 * The credential boundary.
 *
 * WHY THIS FILE EXISTS -- THE MOST IMPORTANT DECISION IN THE FRONTEND
 * ==================================================================
 * The Go API authenticates with `Authorization: Bearer <key>`. The browser must never hold that key,
 * and the tempting version is fatal:
 *
 *     // WRONG. NEXT_PUBLIC_* is INLINED INTO THE CLIENT BUNDLE at build time.
 *     fetch(url, { headers: { Authorization: `Bearer ${process.env.NEXT_PUBLIC_API_KEY}` } })
 *
 * That key then lives in the JavaScript bundle, in every visitor's Network tab, and in view-source.
 * Phase 5 compared keys in constant time against a SHA-256 digest and refused any key under 16
 * characters; shipping it to the browser makes all of that decorative. The strength of a secret is
 * irrelevant once everyone has it.
 *
 * So the browser talks to THIS route, same-origin and unauthenticated, and this route talks to the Go
 * API with the key. Three things follow for free:
 *
 *   - CORS disappears. Same origin means no preflight and no Access-Control-Allow-* on the Go side,
 *     which is a security control we then do not have to configure correctly.
 *   - The Go API needs no public exposure at all. It can stay a ClusterIP service, reachable only
 *     from inside the cluster, which is a far better default than "internet-facing but authenticated".
 *   - There is now one place to add per-session throttling, audit logging or a tenant check later.
 *
 * WHY A ROUTE HANDLER AND NOT A next.config REWRITE
 * A rewrite is fewer lines and does not work: it is transparent proxying at the routing layer and
 * cannot attach a header. The browser would still have to supply the credential. The proxy exists
 * precisely to HOLD something, which needs code.
 *
 * WHY NOT SERVER ACTIONS
 * Server Actions are POSTs designed for mutations, and every endpoint here is a GET. Using them would
 * make cacheable reads uncacheable and lose the HTTP semantics -- status codes, Cache-Control -- that
 * the Go API is careful about.
 *
 * HOW IT COMMUNICATES WITH THE REST OF THE APP
 * src/lib/api/client.ts is the only caller. Nothing else constructs a URL for the Go API, so this
 * file plus that one are the entire surface between the browser and the backend.
 */

/**
 * apiBaseURL is where the Go API lives, from the SERVER's point of view.
 *
 * Note the absent NEXT_PUBLIC_ prefix on both variables in this file. That is the whole mechanism:
 * Next.js inlines only NEXT_PUBLIC_* into the client bundle, so a plain name is readable on the server
 * and simply `undefined` in the browser. A single misplaced prefix here would leak the key, which is
 * why they are read in one place rather than wherever they happen to be needed.
 */
function apiBaseURL(): string {
  return process.env.KCA_API_URL ?? "http://localhost:8080";
}

/**
 * upstreamPaths is an ALLOW-LIST of the paths this proxy will forward.
 *
 * WHY AN ALLOW-LIST RATHER THAN FORWARDING WHATEVER ARRIVES
 * ---------------------------------------------------------
 * A catch-all route with `[...path]` will happily forward anything, and this proxy adds a credential
 * to every request it makes. Without a list, it is an authenticated open relay into the API: a browser
 * could ask for any path, including endpoints added later that were never meant to be public, and the
 * proxy would attach the key and oblige.
 *
 * That is the same reasoning the Go side applies to SQL identifiers, and to the RBAC grants: an
 * allow-list fails by refusing something new, and a deny-list fails by permitting it. Here the
 * asymmetry is sharper than usual, because the thing being permitted comes with our credential
 * attached.
 *
 * PREFIXES, NOT EXACT PATHS, because query strings and future sub-paths are legitimate. Anchored with
 * a check that the remainder starts with "?" or is empty, so "/api/v1/nodes-secret" cannot ride in on
 * the "/api/v1/nodes" prefix.
 */
const upstreamPaths = [
  "/api/v1/nodes",
  "/api/v1/namespaces",
  "/api/v1/pods",
  "/api/v1/costs/summary",
  "/api/v1/costs/trend",
  "/api/v1/allocations",
  "/api/v1/recommendations",
  "/api/v1/reports/monthly",
  // /healthz and /readyz are deliberately ABSENT.
  //
  // internal/health documents that /readyz includes dependency error strings and must never be
  // exposed publicly. Proxying it would make it public through the dashboard's own origin, which is
  // exactly the exposure that note warns about -- and the browser has no use for it anyway.
  //
  // /version is absent for the reason the Phase 7 audit established: build and commit are what an
  // attacker fingerprints for known CVEs, and nothing in the UI needs them.
] as const;

function isAllowed(path: string): boolean {
  return upstreamPaths.some(
    (allowed) => path === allowed || path.startsWith(`${allowed}?`),
  );
}

/**
 * GET forwards a read to the Go API with the credential attached.
 *
 * ONLY GET IS EXPORTED. A route handler serves exactly the methods it exports, so POST, PUT, PATCH and
 * DELETE all get an automatic 405 rather than needing a rejection branch. That mirrors the API itself:
 * every endpoint is read-only, and internal/httpapi/router.go makes the same point about the service
 * being structurally incapable of mutating what it observes. The proxy inherits that property by
 * exporting one function.
 */
export async function GET(
  request: NextRequest,
  context: { params: Promise<{ path: string[] }> },
): Promise<Response> {
  // `params` is a Promise in Next 15. Awaited rather than destructured, because a dynamic route's
  // params can arrive after the request begins streaming.
  const { path } = await context.params;

  const upstreamPath = `/api/v1/${path.join("/")}`;
  const search = request.nextUrl.search;
  const target = `${upstreamPath}${search}`;

  if (!isAllowed(target)) {
    // 404, NOT 403.
    //
    // A 403 confirms the path exists and is merely forbidden, which tells a prober where to look
    // next. From the browser's perspective this proxy genuinely has no such route, so 404 is both
    // the more accurate answer and the less useful one to an attacker.
    return jsonError(404, "not_found", "no such endpoint");
  }

  const apiKey = process.env.KCA_API_KEY;

  const headers: Record<string, string> = { Accept: "application/json" };
  if (apiKey) {
    headers.Authorization = `Bearer ${apiKey}`;
  }
  // A MISSING KEY IS NOT AN ERROR HERE, deliberately.
  //
  // config.Validate permits an unauthenticated API outside production, so `make run-api` and this
  // dashboard both work with no ceremony in development. Failing here would break the local loop for
  // a control that is not enabled locally. If the API does require a key and none is configured, the
  // API answers 401 and that status is forwarded unchanged -- which is a far more useful error than
  // anything this file could invent, because it comes from the thing doing the rejecting.

  let upstream: Response;
  try {
    upstream = await fetch(`${apiBaseURL()}${target}`, {
      headers,
      // NO CACHING AT THIS LAYER.
      //
      // The Go API already sets `Cache-Control: private, max-age=60` and `Vary: Authorization` on
      // cost responses, having thought about it (see setCostCacheHeaders). Next.js's fetch cache
      // would layer a second, longer-lived cache with different rules on top -- and it is shared
      // across users of this server, which is precisely what `private` tells intermediaries not to
      // do. One cache, owned by the layer that understands the data.
      cache: "no-store",
      // A bound, so a hung API surfaces as a 504 rather than holding a Next.js worker open until the
      // platform's own timeout. 30s is comfortably above the slowest real query -- a 400-day summary
      // -- and far below any patience a human has.
      signal: AbortSignal.timeout(30_000),
    });
  } catch (err) {
    const timedOut = err instanceof Error && err.name === "TimeoutError";
    // Logged server-side with the target, because that detail is diagnostic and must not go to the
    // browser: the upstream URL reveals internal topology.
    console.error("[kca-proxy] upstream request failed", {
      target,
      error: err instanceof Error ? err.message : String(err),
    });
    return timedOut
      ? jsonError(504, "upstream_timeout", "the cost API did not respond in time")
      : jsonError(502, "upstream_unreachable", "the cost API is unreachable");
  }

  // The upstream body and status are forwarded UNCHANGED.
  //
  // Including the 400s. Phase 5 put real work into validation errors that name every bad parameter
  // with a reason, and a proxy that replaced them with a generic "bad request" would throw that away
  // -- the UI could no longer highlight the offending input. A proxy's job is to add a credential,
  // not to reinterpret the answer.
  const body = await upstream.text();

  const responseHeaders = new Headers({
    "Content-Type": upstream.headers.get("Content-Type") ?? "application/json",
  });
  // Cache-Control is passed through so the BROWSER still honours the API's 60 seconds. `Vary:
  // Authorization` is deliberately NOT forwarded: from the browser's side there is no Authorization
  // header to vary on, and advertising one would be describing a request it never made.
  const cacheControl = upstream.headers.get("Cache-Control");
  if (cacheControl) responseHeaders.set("Cache-Control", cacheControl);

  return new Response(body, { status: upstream.status, headers: responseHeaders });
}

/** jsonError shapes a proxy-generated failure like the API's own error envelope. */
function jsonError(status: number, code: string, message: string): Response {
  // The SAME shape internal/httpapi/respond.go uses: { error: { code, message } }.
  //
  // Not for tidiness. The client has one error path, and a proxy failure that arrived in a different
  // shape would need a second one -- so every consumer would have to sniff which layer failed before
  // it could show anything. Two error shapes means every caller handles neither properly.
  return new Response(JSON.stringify({ error: { code, message } }), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}
