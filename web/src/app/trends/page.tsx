import { api, serverBaseURL } from "@/lib/api/client";
import { defaultRangeFor, resolveRange } from "@/lib/range";
import { MAX_SERIES } from "@/lib/viz/palette";
import { TrendsView } from "./TrendsView";

export const dynamic = "force-dynamic";

export default async function TrendsPage() {
  const rangeKey = defaultRangeFor("trends");
  const range = resolveRange(rangeKey);

  const initialData = await api.trend(
    { ...range, interval: "day", group_by: "namespace", compare: "previous_period", limit: MAX_SERIES },
    serverBaseURL(),
  );

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-lg font-semibold" style={{ color: "var(--text-primary)" }}>Trends</h1>
        <p className="mt-1 text-sm" style={{ color: "var(--text-secondary)" }}>
          Cost through time. Daily and coarser buckets read the pre-aggregated rollup, which is roughly
          293× smaller than the raw samples; hourly reads the samples directly. The response says which,
          and this page shows it.
        </p>
      </div>
      <TrendsView initialData={initialData} initialRange={rangeKey} />
    </div>
  );
}
