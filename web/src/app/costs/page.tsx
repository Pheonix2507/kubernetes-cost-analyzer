import { api, serverBaseURL, type GroupBy } from "@/lib/api/client";
import { defaultRangeFor, resolveRange } from "@/lib/range";
import { CostsTable } from "./CostsTable";

/**
 * The costs page: a Server Component that fetches, wrapping a Client Component that interacts.
 *
 * THIS IS THE PATTERN THE WHOLE PHASE IS ABOUT
 * ============================================
 * The split is not arbitrary. Each half does the thing the other cannot:
 *
 *   THIS FILE (server)          holds the API key, fetches, ships no JavaScript
 *   CostsTable (client)         holds selector state, re-queries, caches
 *
 * The server's result is passed down as `initialData`, so the first paint has rows in it. The client
 * takes over from there. The alternative -- making the whole page a Client Component -- would mean an
 * empty first render, a fetch, then a re-render, and the API key would have to reach the browser to make
 * that fetch at all.
 *
 * The alternative in the other direction -- all server, re-fetching the page on every selector change --
 * would be a full round trip and a full re-render to change one dropdown.
 */
export const dynamic = "force-dynamic";

export default async function CostsPage() {
  const rangeKey = defaultRangeFor("costs");
  const groupBy: GroupBy = "namespace";
  const range = resolveRange(rangeKey);

  // Fetched with the SAME parameters the client will use for its first key, so the hydrated cache entry
  // matches and no request is made on mount. A mismatch here -- a different limit, a different sort --
  // would make the client refetch immediately and the server's work would be wasted.
  const initialData = await api.costSummary(
    { ...range, group_by: groupBy, sort: "total_cost", order: "desc", limit: 100 },
    serverBaseURL(),
  );

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-lg font-semibold" style={{ color: "var(--text-primary)" }}>
          Costs
        </h1>
        <p className="mt-1 text-sm" style={{ color: "var(--text-secondary)" }}>
          Spend grouped by any dimension. Billed on <code>max(request, usage)</code>, so a container that
          declares nothing is still charged for what it uses.
        </p>
      </div>

      <CostsTable initialData={initialData} initialGroupBy={groupBy} initialRange={rangeKey} />
    </div>
  );
}
