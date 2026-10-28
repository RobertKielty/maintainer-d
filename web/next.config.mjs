// Plain JS rather than TypeScript: `next start` loads this file at runtime,
// and the production image prunes devDependencies (including typescript).
// Dockerfile.web copies it into the runner stage; without it the image
// optimizer rejects every remote avatar URL.

/** @type {import('next').NextConfig} */
const nextConfig = {
  distDir: process.env.NEXT_DIST_DIR || ".next",
  turbopack: {
    root: process.env.TURBOPACK_ROOT || "./",
  },
  outputFileTracingRoot: process.env.OUTPUT_FILE_TRACING_ROOT,
  images: {
    remotePatterns: [
      {
        protocol: "https",
        hostname: "github.com",
        pathname: "/**",
      },
      {
        protocol: "https",
        hostname: "avatars.githubusercontent.com",
        pathname: "/**",
      },
    ],
  },
};

export default nextConfig;
