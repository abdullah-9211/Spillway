"use client";

import type { User } from "@/lib/api";

export function initials(name: string): string {
  const parts = name.split(/[\s._-]+/).filter(Boolean);
  const letters = parts.length > 1 ? parts[0][0] + parts[1][0] : name.slice(0, 2);
  return letters.toUpperCase();
}

/** Who is signed in, their role, and the way out. */
export function UserChip({ user }: { user: User }) {
  return (
    <div className="user">
      <span className="av" aria-hidden="true">
        {initials(user.username)}
      </span>
      <span className="user__name">{user.username}</span>
      <span className="role" data-role={user.role}>
        {user.role}
      </span>
      <form action="/api/auth/logout" method="post" className="user__out">
        <button type="submit" className="link-btn">
          Sign out
        </button>
      </form>
    </div>
  );
}
