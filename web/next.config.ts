import type { NextConfig } from "next";

const config: NextConfig = {
  // Fail the build on a type error rather than shipping one.
  //
  // This is the default, and it is stated explicitly because the escape hatch
  // (typescript.ignoreBuildErrors) is a tempting thing to reach for under deadline and a
  // terrible thing to leave behind. The Go side runs `make check` in CI for the same reason.
  typescript: { ignoreBuildErrors: false },
  eslint: { ignoreDuringBuilds: false },

  // No `images` or `rewrites` config, deliberately.
  //
  // A rewrite proxying /api/kca/* straight to the Go API would be fewer lines than the route
  // handler in src/app/api/kca -- and it would defeat the point. A rewrite is transparent: it
  // cannot add an Authorization header, so the browser would still have to supply the key.
  // The proxy exists to HOLD a credential, which needs server code rather than config.
  reactStrictMode: true,
};

export default config;
