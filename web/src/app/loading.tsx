/**
 * The route-level loading state.
 *
 * A Server Component with no "use client": it is static markup that Next.js streams IMMEDIATELY while
 * the page's own data is still being fetched, which is how a Suspense boundary turns a blank wait into
 * a visible layout.
 *
 * WHY SKELETONS RATHER THAN A SPINNER. A spinner says "something is happening" and nothing else. A
 * skeleton in the shape of the eventual content says what is coming and reserves its space, so the page
 * does not jump when data arrives -- and layout shift after a wait is more annoying than the wait.
 */
export default function Loading() {
  return (
    <div className="space-y-4" aria-busy="true" aria-live="polite">
      <span className="sr-only">Loading cost data…</span>
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        {[0, 1, 2, 3].map((i) => (
          <div
            key={i}
            className="h-24 animate-pulse rounded-lg border"
            style={{ background: "var(--surface-1)", borderColor: "var(--border)" }}
          />
        ))}
      </div>
      <div
        className="h-80 animate-pulse rounded-lg border"
        style={{ background: "var(--surface-1)", borderColor: "var(--border)" }}
      />
    </div>
  );
}
