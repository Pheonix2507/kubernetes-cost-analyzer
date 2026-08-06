"use client";

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";

/**
 * The TanStack Query provider.
 *
 * WHY "use client" IS THE FIRST LINE
 * ==================================
 * QueryClientProvider is React Context, and Context requires the component tree to be interactive --
 * it holds mutable state and re-renders subscribers. Server Components render once to a stream and
 * cannot do either, so a provider is necessarily a Client Component.
 *
 * This is the boundary itself: everything below inherits client-ness, which is why the provider wraps
 * as little as possible in layout.tsx. Putting `"use client"` on the layout instead would make every
 * page a Client Component and ship the whole dashboard's JavaScript to the browser.
 *
 * WHY useState AND NOT A MODULE-LEVEL CLIENT
 * ==========================================
 * The obvious version is a bug that only appears in production:
 *
 *     const queryClient = new QueryClient();   // WRONG on the server
 *     export function Providers({ children }) {
 *       return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
 *     }
 *
 * A module-level instance is created ONCE PER SERVER PROCESS and therefore SHARED BY EVERY USER. One
 * visitor's cached cost data is served to the next, which for this application means one team's spend
 * shown to another -- the exact leak `Cache-Control: private` exists to prevent, reintroduced above
 * the HTTP layer where that header cannot help.
 *
 * It is invisible in development, because a single developer looks like one user.
 *
 * useState with an initialiser gives one client per component instance: fresh per request on the
 * server, stable across re-renders in the browser. `useState(() => new QueryClient())` rather than
 * `useState(new QueryClient())` matters too -- the second form constructs a client on every render and
 * throws it away, which is wasteful rather than wrong, but it is the same class of mistake.
 */
export function Providers({ children }: { children: ReactNode }) {
  const [queryClient] = useState(
    () =>
      new QueryClient({
        defaultOptions: {
          queries: {
            /**
             * staleTime matches the API's own Cache-Control: 60 seconds.
             *
             * DELIBERATELY THE SAME NUMBER. The collector writes every five minutes, so a response is
             * at most that stale anyway, and setCostCacheHeaders already chose 60s as comfortably
             * inside that. A shorter staleTime here would refetch data the browser cache would serve
             * from memory -- so the request is made, satisfied locally, and the render happens twice
             * for no new information.
             *
             * The default is 0, which means every mount refetches. On a dashboard where a user
             * navigates between five pages that read overlapping data, that is five times the queries
             * for one page of information.
             */
            staleTime: 60_000,

            /**
             * No refetch on window focus.
             *
             * The default is `true` and it is right for a chat app and wrong here. Cost data changes
             * every five minutes; alt-tabbing back to a dashboard should not re-run a 400-day
             * aggregate. Worse, a figure that silently changes while somebody is reading it out to
             * a colleague is a genuine problem rather than a stale-data one.
             */
            refetchOnWindowFocus: false,

            /**
             * Retry once, and NEVER on a 4xx.
             *
             * A 400 means the request was wrong and will be wrong every time -- retrying is pure delay
             * before showing the user the validation error that would have helped them immediately.
             * A 401 will not fix itself either. Only 5xx and network failures are worth a second
             * attempt.
             *
             * The default of three retries with backoff turns a mistyped filter into several seconds
             * of spinner followed by the same error.
             */
            retry: (failureCount, error) => {
              const status = (error as { status?: number }).status;
              if (status !== undefined && status >= 400 && status < 500) return false;
              return failureCount < 1;
            },
          },
        },
      }),
  );

  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}
