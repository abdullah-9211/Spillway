import type { Metadata } from "next";
import { cookies } from "next/headers";
import type { ReactNode } from "react";
import { THEME_COOKIE, parseTheme, themeBootScript } from "@/lib/theme";
import "@/styles/tokens.css";
import "@/styles/app.css";

export const metadata: Metadata = {
  title: { default: "Spillway", template: "%s · Spillway" },
  description: "Dashboard for the Spillway agent runtime and model gateway.",
};

export default async function RootLayout({ children }: { children: ReactNode }) {
  // A saved choice is rendered by the server. With none, the boot script picks the system preference before paint.
  const saved = parseTheme((await cookies()).get(THEME_COOKIE)?.value);
  return (
    <html lang="en" data-theme={saved ?? undefined} suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: themeBootScript }} />
        <link rel="preconnect" href="https://fonts.googleapis.com" />
        <link rel="preconnect" href="https://fonts.gstatic.com" crossOrigin="" />
        {/* eslint-disable-next-line @next/next/no-page-custom-font */}
        <link
          rel="stylesheet"
          href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600&family=Cormorant+Garamond:wght@500;600&family=JetBrains+Mono:wght@400;500&display=swap"
        />
      </head>
      <body>
        <a className="skip" href="#main">
          Skip to content
        </a>
        {children}
      </body>
    </html>
  );
}
