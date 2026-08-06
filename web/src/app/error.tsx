"use client";

/**
 * The error boundary.
 *
 * MUST be a Client Component: React error boundaries work by catching a thrown render and re-rendering
 * with state, and state needs interactivity. There is no server equivalent.
 *
 * WHY THIS FILE EXISTS AT ALL. Without it, a thrown fetch in any page produces Next.js's default error
 * screen -- which in production says "Application error: a client-side exception has occurred" and
 * nothing else. For a dashboard whose backend can legitimately be down, the difference between that and
 * "the cost API is unreachable" is the difference between a support ticket and a glance at the logs.
 */
export default function Error({ error, reset }: { error: Error; reset: () => void }) {
  return (
    <div
      className="rounded-lg border p-6"
      style={{ background: "var(--surface-1)", borderColor: "var(--border)" }}
    >
      <h1 className="text-base font-semibold" style={{ color: "var(--text-primary)" }}>
        This page could not load
      </h1>
      {/*
       * The message is shown. It comes from ApiError and is already safe for a user: the Go API masks
       * driver detail server-side before it ever reaches us (see logError in internal/httpapi), and the
       * proxy logs upstream URLs rather than forwarding them. So there is nothing here to leak, and
       * hiding it would only cost the reader the one useful sentence.
       */}
      <p className="mt-2 text-sm" style={{ color: "var(--text-secondary)" }}>
        {error.message}
      </p>
      <p className="mt-3 text-xs" style={{ color: "var(--text-muted)" }}>
        If the cost API is not running, start it with <code>make run-api</code>. A collector that has
        never run leaves the database empty, which shows as zero cost rather than an error.
      </p>
      <button
        type="button"
        onClick={reset}
        className="mt-4 rounded border px-3 py-1.5 text-sm"
        style={{ borderColor: "var(--border)", color: "var(--text-primary)" }}
      >
        Try again
      </button>
    </div>
  );
}
