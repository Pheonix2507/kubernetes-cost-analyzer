import type { Metadata } from "next";
import Link from "next/link";
import { Providers } from "@/lib/query/provider";
import "./globals.css";

export const metadata: Metadata = {
  title: "Kubernetes Cost Analyzer",
  description: "Attributes Kubernetes spend to the teams and workloads that cause it.",
};

/**
 * The root layout.
 *
 * NOTE WHAT IS AND IS NOT A CLIENT COMPONENT HERE
 * ===============================================
 * This file has no "use client". It is a Server Component, so the nav and the page chrome ship zero
 * JavaScript -- they are static HTML that never changes after render.
 *
 * `Providers` is the client boundary, and it wraps `{children}` as tightly as possible. Putting
 * "use client" on this file instead would be one line shorter and would make EVERY page a Client
 * Component: the whole dashboard's JavaScript would ship to the browser, and pages that need no
 * interactivity at all would hydrate for nothing. The boundary's position is the performance decision.
 */
const nav = [
  { href: "/", label: "Overview" },
  { href: "/costs", label: "Costs" },
  { href: "/trends", label: "Trends" },
  { href: "/recommendations", label: "Recommendations" },
  { href: "/reports", label: "Reports" },
];

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body className="min-h-screen">
        <header className="border-b" style={{ borderColor: "var(--border)", background: "var(--surface-1)" }}>
          <div className="mx-auto flex max-w-7xl flex-wrap items-center gap-x-6 gap-y-2 px-4 py-3">
            <span className="text-sm font-semibold" style={{ color: "var(--text-primary)" }}>
              Kubernetes Cost Analyzer
            </span>
            <nav aria-label="Main">
              <ul className="flex flex-wrap gap-4">
                {nav.map((item) => (
                  <li key={item.href}>
                    {/* next/link prefetches and navigates client-side without making this a Client
                        Component -- Link is a server-renderable component whose behaviour is supplied
                        by the framework's router. */}
                    <Link
                      href={item.href}
                      className="text-sm hover:underline"
                      style={{ color: "var(--text-secondary)" }}
                    >
                      {item.label}
                    </Link>
                  </li>
                ))}
              </ul>
            </nav>
          </div>
        </header>

        <Providers>
          <main className="mx-auto max-w-7xl px-4 py-6">{children}</main>
        </Providers>
      </body>
    </html>
  );
}
