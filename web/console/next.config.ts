import type { NextConfig } from "next";

// In development the console runs on :3000 and forwards API calls to the
// gateway. In production the gateway serves both, so no rewrite is needed.
const apiOrigin = process.env.API_ORIGIN;

const config: NextConfig = {
  reactStrictMode: true,
  poweredByHeader: false,
  // Don't generate AGENTS.md / CLAUDE.md into the repo on `next dev`.
  agentRules: false,
  async rewrites() {
    if (!apiOrigin) return [];
    return [
      { source: "/v1/:path*", destination: `${apiOrigin}/v1/:path*` },
      { source: "/install.sh", destination: `${apiOrigin}/install.sh` },
    ];
  },
};

export default config;
