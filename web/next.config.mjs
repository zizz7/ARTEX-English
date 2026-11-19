/** @type {import('next').NextConfig} */

// Static export: `NEXT_EXPORT=1 next build` emits a pure static directory to web/out, which can be
// dropped straight into the nginx web root. Development (next dev) does not set this variable, keeping the /api proxy and hot reload.
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel demo: the whole site runs on mock data, no backend, no /api proxy needed.
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

const nextConfig = {
  reactCompiler: true,
  // Allow dev resources (HMR) to be accessed from LAN IPs; add or remove as needed.
  // During dev, allow any IPv4 origin to access /_next/* and HMR (unaffected by LAN IP changes).
  // Note: for security, Next forbids a bare "*"; use segmented wildcards instead. "*.*.*.*" matches any IPv4.
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // Pure static export: no Node runtime; images are not optimized; each route emits <route>/index.html.
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock demo: no backend, no /api proxy needed.
          images: { unoptimized: true },
        }
      : {
          // Development: proxy /api/* to the Go backend (default :8787, overridable via AUTOPENTEST_API).
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

export default nextConfig;
