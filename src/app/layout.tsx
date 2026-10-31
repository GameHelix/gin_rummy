import type { Metadata, Viewport } from "next";
import "./globals.css";

/** The tab icon: a neon card drawn inline, so there is no icon file to fetch. */
const ICON = `data:image/svg+xml,${encodeURIComponent(
  '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">' +
    '<rect x="12" y="4" width="40" height="56" rx="7" fill="#0b0a1f" stroke="#22d3ee" stroke-width="3"/>' +
    '<path d="M32 17 L43 32 L32 47 L21 32 Z" fill="#fb7185"/>' +
    "</svg>"
)}`;

export const metadata: Metadata = {
  title: "Gin Rummy — knock, gin and undercut against the CPU",
  description:
    "Neon Gin Rummy built with Next.js and TypeScript. Draw, meld sets and runs, knock at 10 or less or go for gin against an Easy, Medium or Hard CPU, with lay-offs, undercuts, big gin and full scoring to 100.",
  icons: { icon: ICON },
};

export const viewport: Viewport = {
  themeColor: "#070610",
  width: "device-width",
  initialScale: 1,
  viewportFit: "cover",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
