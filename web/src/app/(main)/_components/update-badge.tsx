"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * The "new version available" badge in the top bar: checked once on full page load; when an update exists it lights up next to the version number,
 * and clicking it jumps to the "Version & Updates" card on the system settings page.
 *
 * The backend caches GitHub query results for 30 minutes, so querying once per mount here is safe
 * -- unauthenticated GitHub API calls are limited to 60/hour/IP; without that cache layer, opening a few tabs
 * would exhaust the quota, and the check would fail exactly when you actually want to update.
 *
 * Query failures are always silent: the top bar is not the place for errors; users will see the reason when they click "Check for updates" on the settings page.
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update already incorporates the "versions are comparable" check, so dev builds never light up this badge.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // Stay silent: no network / GitHub rate limiting must not pop errors in the top bar.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`New version ${latest} found -- click to update`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* Pulsing dot: the top bar has many elements and plain text is easy to miss; the animation makes it visible at a glance. */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">New version {latest}</span>
      <span className="sm:hidden">New version</span>
    </Link>
  );
}
