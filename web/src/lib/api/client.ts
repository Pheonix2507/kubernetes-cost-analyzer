import type { paths } from "./schema";

/**
 * The typed API client.
 *
 * WHY THIS FILE EXISTS
 * ====================
 * One place that knows how to talk to the backend, so nothing else builds a URL or interprets a
 * status code. Two consequences worth the file on their own:
 *
 *   - The proxy in src/app/api/kca is the only route to the Go API, and this is its only caller. Those
 *     two files are therefore the complete surface between browser and backend, which is a property
 *     you can verify by reading two files rather than by grepping for `fetch`.
 *   - Every response type comes from schema.d.ts, which is GENERATED from api/openapi.yaml. So a Go
 *     handler that changes its response shape breaks `pnpm typecheck` rather than breaking a page at
 *     runtime for a user.
 *
 * That second point is the payoff for the drift tests on the Go side. openapi_test.go asserts the spec
 * matches the code's allow-lists; this asserts the frontend matches the spec. The chain runs
 * SQL -> Go -> spec -> TypeScript, and each link is checked by something that fails loudly.
 *
 * HOW COMPANIES USUALLY DO THIS
 * Three approaches. Hand-written types, which drift silently and are the most common. A generated
 * client (openapi-generator, orval) that produces both types AND fetch functions -- more automated,
 * and it generates a lot of code you did not write and cannot easily read. Or generated TYPES with a
 * hand-written fetch layer, which is this: the types are the part that must never drift, and the
 * ~40 lines of fetching are the part worth being able to read.
 */

/**
 * ApiError carries a failed response.
 *
 * A class rather than a returned union, so callers can `throw`/`catch` and TanStack Query can classify
 * it. Query needs an exception to know a fetch failed at all -- returning an error object would look
 * like success and get cached as data.
 */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    /**
     * fields names each invalid parameter, when the API sent them.
     *
     * Carried through rather than flattened into the message, because Phase 5 built these
     * specifically so a UI could highlight the offending input. Collapsing them to a string would
     * discard the only part a form can act on.
     */
    readonly fields?: { field: string; reason: string }[],
  ) {
    super(message);
    this.name = "ApiError";
  }

  /**
   * isValidation distinguishes "you asked wrongly" from "we failed".
   *
   * The two need different UI: a 400 means fix the filter and the user can, a 500 means retry and they
   * cannot. Collapsing them into "something went wrong" is how a dashboard becomes unusable at the
   * exact moment somebody needs it.
   */
  get isValidation(): boolean {
    return this.status === 400;
  }
}

/** basePath is the proxy mount point. Relative, so it works on any host without configuration. */
const basePath = "/api/kca";

/** ParamValue is what can appear in a query string. */
type ParamValue = string | number | boolean | undefined;

/**
 * ParamShape constrains every VALUE of a params object without demanding an index signature.
 *
 * The obvious signature -- `params: Record<string, ParamValue>` -- does not compile against an
 * interface, because TypeScript will not assign an interface to a Record: an interface has known keys
 * and no index signature, so it cannot promise anything about keys nobody declared. That is a real
 * guarantee rather than pedantry, and the two usual escapes both give something up. Adding
 * `[k: string]: ParamValue` to Filters would let any typo through as a valid filter. Casting to
 * Record inside would silence the check that a value is serialisable at all.
 *
 * This self-referential mapped type says "for every key P actually has, its value must be a
 * ParamValue" -- which checks exactly the property that matters and stays silent about keys that do
 * not exist. So `{ namespace: "x" }` passes, `{ namespace: { a: 1 } }` is a compile error rather than
 * the string "[object Object]" in a query string, and Filters needs no index signature.
 */
type ParamShape<P> = { [K in keyof P]: ParamValue };

/**
 * request performs a GET through the proxy.
 *
 * `baseURL` exists for ONE reason: Server Components run on a server that has no notion of "this
 * page's origin", so a relative fetch has nothing to resolve against and throws. The browser resolves
 * relative URLs against the current page; Node does not. This is the single most common Next.js
 * data-fetching mistake, and it fails only in the RSC path -- so it looks like it works in
 * development until a page is server-rendered.
 */
async function request<T, P extends ParamShape<P>>(
  path: string,
  params: P,
  baseURL?: string,
): Promise<T> {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    // Undefined is DROPPED rather than serialised. `?namespace=undefined` would be sent as the literal
    // string "undefined" and filter for a namespace with that name -- returning an empty result that
    // looks exactly like "this namespace costs nothing".
    if (value !== undefined && value !== "") search.set(key, String(value));
  }
  const qs = search.toString();
  const url = `${baseURL ?? ""}${basePath}${path}${qs ? `?${qs}` : ""}`;

  const res = await fetch(url, {
    headers: { Accept: "application/json" },
    // NO Authorization HEADER HERE, and its absence is the design rather than an omission. The proxy
    // adds it. If this line ever needs a credential, something has gone wrong upstream of it.
    cache: "no-store",
  });

  if (!res.ok) {
    // The error envelope is parsed rather than the status guessed at, so the API's own message reaches
    // the user. A generic string keyed off the status would discard the part that says WHICH parameter
    // was wrong.
    let code = "unknown";
    let message = `request failed with status ${res.status}`;
    let fields: { field: string; reason: string }[] | undefined;
    try {
      const body = (await res.json()) as {
        error?: { code?: string; message?: string; fields?: { field: string; reason: string }[] };
      };
      if (body.error) {
        code = body.error.code ?? code;
        message = body.error.message ?? message;
        fields = body.error.fields;
      }
    } catch {
      // A non-JSON error body -- a proxy timeout page, an nginx 502 -- is not worth failing over. The
      // status is already the useful part, and swallowing this keeps a bad gateway from becoming an
      // unhandled parse exception in a chart component.
    }
    throw new ApiError(res.status, code, message, fields);
  }

  return (await res.json()) as T;
}

// =============================================================================
// Response types, extracted from the generated schema
// =============================================================================
//
// These aliases exist so components import `CostSummaryResponse` rather than the unreadable
// `paths["/api/v1/costs/summary"]["get"]["responses"]["200"]["content"]["application/json"]`. The
// indirection is worth it twice over: the names are legible, and if a path moves in the spec exactly
// one line changes here instead of every call site.

type JSON200<P extends keyof paths> = paths[P] extends {
  get: { responses: { 200: { content: { "application/json": infer T } } } };
}
  ? T
  : never;

export type CostSummaryResponse = JSON200<"/api/v1/costs/summary">;
export type TrendResponse = JSON200<"/api/v1/costs/trend">;
export type RecommendationsResponse = JSON200<"/api/v1/recommendations">;
export type MonthlyReportsResponse = JSON200<"/api/v1/reports/monthly">;
export type NodesResponse = JSON200<"/api/v1/nodes">;
export type AllocationsResponse = JSON200<"/api/v1/allocations">;

export type CostSummaryRow = NonNullable<CostSummaryResponse["items"]>[number];
export type TrendSeries = NonNullable<TrendResponse["series"]>[number];
export type TrendPoint = NonNullable<TrendSeries["points"]>[number];
export type Recommendation = NonNullable<RecommendationsResponse["items"]>[number];
export type MonthlyReport = NonNullable<MonthlyReportsResponse["items"]>[number];

/** GroupBy is the dimension a cost query aggregates over. */
export type GroupBy =
  | "namespace"
  | "team"
  | "environment"
  | "cost_centre"
  | "workload"
  | "node"
  | "instance_type"
  | "capacity_type"
  | "pod"
  | "container";

/** Interval is a trend bucket width. */
export type Interval = "hour" | "day" | "week" | "month";

/**
 * Filters mirrors the API's filter set.
 *
 * All optional, all strings. Named to match the query parameters exactly so the mapping is the
 * identity function -- a renamed field here would need a translation layer, and a translation layer
 * is where a filter silently stops being applied.
 */
export interface Filters {
  namespace?: string;
  team?: string;
  environment?: string;
  cost_centre?: string;
  workload_kind?: string;
  workload_name?: string;
  node?: string;
  instance_type?: string;
  capacity_type?: string;
  estimated_only?: boolean;
}

// =============================================================================
// The endpoints
// =============================================================================
//
// Each takes an optional baseURL for the Server Component case. Explicit rather than read from a
// module-level variable, because a module-level "am I on the server" flag is exactly the kind of
// state that is correct in development and wrong under a production build.
//
// NOTE THE RETURN TYPE ANNOTATIONS rather than `request<CostSummaryResponse>(...)`. `request` has two
// type parameters -- the response and the params shape -- and TypeScript infers all or none, so
// naming the response explicitly would force every call site to spell out the params type too. The
// annotation gives TypeScript a contextual type to infer the response FROM, so both are checked and
// neither is written twice.

export const api = {
  costSummary: (
    params: {
      from?: string;
      to?: string;
      group_by?: GroupBy;
      sort?: string;
      order?: "asc" | "desc";
      limit?: number;
    } & Filters,
    baseURL?: string,
  ): Promise<CostSummaryResponse> => request("/costs/summary", params, baseURL),

  trend: (
    params: {
      from?: string;
      to?: string;
      interval?: Interval;
      group_by?: GroupBy;
      compare?: "previous_period" | "none";
      limit?: number;
    } & Filters,
    baseURL?: string,
  ): Promise<TrendResponse> => request("/costs/trend", params, baseURL),

  recommendations: (
    params: { from?: string; to?: string; limit?: number } & Filters,
    baseURL?: string,
  ): Promise<RecommendationsResponse> => request("/recommendations", params, baseURL),

  monthlyReports: (
    params: {
      from?: string;
      to?: string;
      scope_kind?: "cluster" | "namespace" | "team";
      scope_value?: string;
      limit?: number;
    },
    baseURL?: string,
  ): Promise<MonthlyReportsResponse> => request("/reports/monthly", params, baseURL),

  nodes: (baseURL?: string): Promise<NodesResponse> => request("/nodes", {}, baseURL),
};

/**
 * serverBaseURL is the absolute origin a Server Component must fetch against.
 *
 * A Server Component fetching "/api/kca/..." throws `Failed to parse URL`, because Node has no
 * ambient origin to resolve a relative path against. This provides one.
 *
 * It reads a non-public variable, so it is server-only by construction: calling it in a Client
 * Component returns the localhost fallback, which fails visibly in development rather than silently
 * in production. Failing loudly in the cheap environment is the point.
 */
export function serverBaseURL(): string {
  return process.env.KCA_SELF_URL ?? "http://localhost:3000";
}
