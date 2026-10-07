import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // The frontend talks to the Go backend over REST + WebSocket at
  // NEXT_PUBLIC_FIXLAB_API_URL (default http://localhost:8080).
  // Standalone output keeps the Docker image small: `node server.js` in
  // .next/standalone serves the whole app.
  output: "standalone",
  reactStrictMode: true,
};

export default nextConfig;
