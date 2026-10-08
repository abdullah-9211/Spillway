"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { KeysIcon, PlaygroundIcon, RunsIcon, UsageIcon } from "./icons";

const ITEMS = [
  { href: "/", label: "Runs", icon: <RunsIcon /> },
  { href: "/usage", label: "Usage and cost", icon: <UsageIcon /> },
  { href: "/keys", label: "API keys", icon: <KeysIcon /> },
  { href: "/playground", label: "Playground", icon: <PlaygroundIcon /> },
];

export function isActive(pathname: string, href: string): boolean {
  return href === "/" ? pathname === "/" || pathname.startsWith("/runs") : pathname === href || pathname.startsWith(href + "/");
}

export function NavLinks() {
  const pathname = usePathname();
  return (
    <nav className="nav" aria-label="Main">
      {ITEMS.map((it) => {
        const on = isActive(pathname, it.href);
        return (
          <Link key={it.href} href={it.href} className={on ? "on" : undefined} aria-current={on ? "page" : undefined}>
            {it.icon}
            {it.label}
          </Link>
        );
      })}
    </nav>
  );
}
