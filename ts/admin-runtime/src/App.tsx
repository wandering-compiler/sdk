// App shell — auth gate (Login when no token / on 401) +
// left nav (declared pages) + content area (List / Detail
// view per route state).
//
// REV-150 P33: navigation moved from in-memory `View` state to
// URL hash routing. The hash is the source of truth; the view
// is derived from it on every change. Three forms:
//   "#/"                              → overview / first page
//   "#/overview"                      → overview (explicit)
//   "#/list/<pageName>"               → list view
//   "#/detail/<pageName>/<rowId>"     → detail view
//   "#/create/<pageName>"             → create form (new row)
// Browser back / forward + page refresh + deep links work out
// of the box.

import { useEffect, useState } from "react";
import {
  AppShell,
  Box,
  Burger,
  Button,
  Collapse,
  Group,
  NavLink,
  Stack,
  Text,
  UnstyledButton,
} from "@mantine/core";
import { useDisclosure } from "@mantine/hooks";

import { Login } from "./Login";
import { ListPage } from "./ListPage";
import { DetailPage } from "./DetailPage";
import { CreatePage } from "./CreatePage";
import { OverviewPage } from "./OverviewPage";
import { Brand, NavMonogram, ThemeToggle } from "./components";
import { IconChevronRight, IconDashboard, IconLogout } from "./icons";
import { pageLabel } from "./format";
import { apiGet } from "./api";
import { getToken, clearToken, pageVisibleTo } from "./auth";
import { overviewIsAvailable } from "./overviewTiles";
import { globalNavItemSlotKey } from "./types";
import type { AdminSpec, SlotRegistry, WhoAmIResp } from "./types";

type View =
  | { kind: "overview" }
  | { kind: "list"; pageName: string }
  | { kind: "detail"; pageName: string; rowId: string }
  | { kind: "create"; pageName: string };

export interface AppProps {
  spec: AdminSpec;
  slots: SlotRegistry;
}

export function App({ spec, slots }: AppProps) {
  const [authState, setAuthState] = useState<"unknown" | "anon" | "authed">("unknown");
  const [whoami, setWhoami] = useState<WhoAmIResp | null>(null);
  const [opened, { toggle }] = useDisclosure();
  // Hash-source-of-truth. setHash fires on every hashchange event
  // (back/forward/manual edit/programmatic navigate). The view is
  // derived from hash + spec on every render.
  const [hash, setHash] = useState<string>(
    typeof window === "undefined" ? "" : window.location.hash,
  );

  const allPages = Object.values(spec.pages);
  // Visible-page filter (REV-150 perm-aware hiding). A page
  // lands in the nav only when the user has the perms required
  // by its list endpoint — pages with no list (rare) OR no
  // required_permissions fall through visible. The detail page
  // is reachable from a list row, so list perm gates the page;
  // the SPA further gates Update / Delete / Action buttons
  // individually.
  const visiblePages = allPages.filter((p) => pageVisibleTo(p, whoami?.permission_ids));
  const pages = visiblePages;
  const firstPage = pages[0];
  // Default landing view: the overview whenever it can render
  // something (declared custom widgets OR default group tiles from
  // the pages), otherwise the first page's list. Used when the hash
  // is empty or names a page the user can't reach.
  const showOverview = overviewIsAvailable(spec);
  const defaultView: View | null = showOverview
    ? { kind: "overview" }
    : firstPage
      ? { kind: "list", pageName: firstPage.name }
      : null;

  useEffect(() => {
    if (typeof window === "undefined") return;
    const onHashChange = () => setHash(window.location.hash);
    window.addEventListener("hashchange", onHashChange);
    return () => window.removeEventListener("hashchange", onHashChange);
  }, []);

  useEffect(() => {
    let cancelled = false;
    if (!getToken()) {
      setAuthState("anon");
      return;
    }
    apiGet<WhoAmIResp>(spec.auth.whoami_endpoint)
      .then((resp) => {
        if (cancelled) return;
        setWhoami(resp);
        setAuthState("authed");
      })
      .catch(() => {
        if (cancelled) return;
        clearToken();
        setAuthState("anon");
      });
    return () => {
      cancelled = true;
    };
  }, [spec.auth.whoami_endpoint]);

  if (authState === "unknown") {
    return <Text>Loading…</Text>;
  }
  if (authState === "anon") {
    return <Login spec={spec} onLogin={() => setAuthState("authed")} />;
  }

  const view = parseHash(hash, spec) ?? defaultView;
  const identity = identityOf(whoami);

  return (
    <AppShell
      header={{ height: 60 }}
      navbar={{ width: 240, breakpoint: "sm", collapsed: { mobile: !opened } }}
      padding="lg"
    >
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between" wrap="nowrap">
          <Group gap="sm" wrap="nowrap" style={{ minWidth: 0 }}>
            <Burger opened={opened} onClick={toggle} hiddenFrom="sm" size="sm" />
            <Brand name={spec.name} />
          </Group>
          <Group gap="xs" wrap="nowrap">
            {identity && (
              <Text fz="sm" c="dimmed" visibleFrom="sm" maw={220} truncate>
                {identity}
              </Text>
            )}
            <ThemeToggle />
            <Button
              variant="default"
              leftSection={<IconLogout size={16} />}
              onClick={() => {
                clearToken();
                setWhoami(null);
                setAuthState("anon");
              }}
            >
              Sign out
            </Button>
          </Group>
        </Group>
      </AppShell.Header>

      <AppShell.Navbar p="md">
        {showOverview && (
          <NavLink
            key="__overview"
            label="Overview"
            leftSection={<IconDashboard size={18} />}
            active={view?.kind === "overview"}
            onClick={() => navigate({ kind: "overview" })}
            mb="md"
          />
        )}
        {spec.nav && spec.nav.length > 0 ? (
          // Grouped nav — render each group as a collapsible
          // section (title + chevron toggle over its pages list).
          // Orphan pages are refused at parse time when nav is
          // declared, so every page lands in exactly one group.
          // Pages the user lacks perms for are filtered out; a
          // group that ends up empty is skipped entirely (avoids
          // dangling group titles with no entries — UX confusion).
          <Stack gap="md">
            {spec.nav.map((g) => {
              const visible = g.pages.filter((pName) => {
                const p = spec.pages[pName];
                return p && pageVisibleTo(p, whoami?.permission_ids);
              });
              if (visible.length === 0) return null;
              return (
                <NavGroup
                  key={g.title}
                  title={g.title}
                  pageNames={visible}
                  spec={spec}
                  activePageName={view?.kind === "list" ? view.pageName : null}
                  onNavigate={(pageName) => navigate({ kind: "list", pageName })}
                />
              );
            })}
          </Stack>
        ) : (
          // Flat nav — page declaration order, already filtered
          // to visiblePages above.
          <Stack gap={2}>
            {pages.map((p) => (
              <NavLink
                key={p.name}
                label={pageLabel(p)}
                leftSection={<NavMonogram label={pageLabel(p)} />}
                active={view?.kind === "list" && view.pageName === p.name}
                onClick={() => navigate({ kind: "list", pageName: p.name })}
              />
            ))}
          </Stack>
        )}
        {/* global:nav:item — consumer-registered nav extension
            (external links, custom views). Renders below every
            declared page so spec-driven entries stay first. */}
        {slots?.[globalNavItemSlotKey()] && slots[globalNavItemSlotKey()]({ spec, whoami })}
      </AppShell.Navbar>

      <AppShell.Main>
        {view == null ? (
          <Text>No pages declared in spec.</Text>
        ) : view.kind === "overview" && showOverview ? (
          <OverviewPage
            spec={spec}
            overview={spec.overview}
            slots={slots}
            whoami={whoami}
            onSelectPage={(pageName) => navigate({ kind: "list", pageName })}
          />
        ) : view.kind === "list" ? (
          <ListPage
            spec={spec}
            page={spec.pages[view.pageName]}
            whoami={whoami}
            slots={slots}
            onSelectRow={(pageName, rowId) => navigate({ kind: "detail", pageName, rowId })}
            onAdd={() => navigate({ kind: "create", pageName: view.pageName })}
          />
        ) : view.kind === "create" ? (
          <CreatePage
            page={spec.pages[view.pageName]}
            whoami={whoami}
            slots={slots}
            onBack={() => navigate({ kind: "list", pageName: view.pageName })}
            onCreated={(newId) =>
              newId
                ? navigate({ kind: "detail", pageName: view.pageName, rowId: newId })
                : navigate({ kind: "list", pageName: view.pageName })
            }
          />
        ) : view.kind === "detail" ? (
          <DetailPage
            spec={spec}
            page={spec.pages[view.pageName]}
            rowId={view.rowId}
            whoami={whoami}
            slots={slots}
            onBack={() => navigate({ kind: "list", pageName: view.pageName })}
            onSelectInlineRow={(childPageName, childId) =>
              navigate({ kind: "detail", pageName: childPageName, rowId: childId })
            }
          />
        ) : null}
      </AppShell.Main>
    </AppShell>
  );
}

// NavGroup renders one collapsible nav section: a clickable
// header (uppercase title + a chevron that rotates open) over
// its pages. Open/closed state persists per group title in
// localStorage so the operator's layout survives reloads;
// groups default to open the first time they're seen.
interface NavGroupProps {
  title: string;
  pageNames: string[];
  spec: AdminSpec;
  activePageName: string | null;
  onNavigate: (pageName: string) => void;
}

function NavGroup({ title, pageNames, spec, activePageName, onNavigate }: NavGroupProps) {
  const [opened, setOpened] = useState(() => readGroupOpened(title, true));
  const toggle = () =>
    setOpened((o) => {
      const next = !o;
      writeGroupOpened(title, next);
      return next;
    });
  return (
    <Stack gap={4}>
      <UnstyledButton
        onClick={toggle}
        aria-expanded={opened}
        w="100%"
        px="xs"
        py={4}
        style={{ borderRadius: "var(--mantine-radius-sm)" }}
      >
        <Group gap={6} wrap="nowrap" justify="space-between">
          <Text size="xs" fw={700} tt="uppercase" c="dimmed" truncate>
            {title}
          </Text>
          <Box
            component="span"
            style={{
              display: "inline-flex",
              color: "var(--mantine-color-dimmed)",
              transition: "transform 150ms ease",
              transform: opened ? "rotate(90deg)" : "rotate(0deg)",
            }}
          >
            <IconChevronRight size={14} />
          </Box>
        </Group>
      </UnstyledButton>
      <Collapse expanded={opened}>
        <Stack gap={2}>
          {pageNames.map((pName) => (
            <NavLink
              key={pName}
              label={pageLabel(spec.pages[pName])}
              leftSection={<NavMonogram label={pageLabel(spec.pages[pName])} />}
              active={activePageName === pName}
              onClick={() => onNavigate(pName)}
            />
          ))}
        </Stack>
      </Collapse>
    </Stack>
  );
}

// navGroupStorageKey namespaces each group's persisted collapse
// state by its title. Distinct admins never share a page, and a
// title is stable across reloads, so the title is a sufficient key.
function navGroupStorageKey(title: string): string {
  return "w17admin:nav:group:" + title;
}

// readGroupOpened / writeGroupOpened persist a group's open state.
// Both are best-effort: localStorage can throw (disabled, privacy
// mode, SSR) and a nav that can't remember its layout must still
// render, so a failure falls back to the default rather than
// surfacing.
function readGroupOpened(title: string, fallback: boolean): boolean {
  if (typeof window === "undefined") return fallback;
  try {
    const v = window.localStorage.getItem(navGroupStorageKey(title));
    if (v === "0") return false;
    if (v === "1") return true;
  } catch {
    // storage unavailable — use the default.
  }
  return fallback;
}

function writeGroupOpened(title: string, opened: boolean): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(navGroupStorageKey(title), opened ? "1" : "0");
  } catch {
    // best-effort; ignore storage failures.
  }
}

// navigate updates the URL hash; the hashchange listener
// re-derives `view` on the next render. Calling navigate with
// the same hash already in place is a no-op (the browser
// suppresses the event).
function navigate(next: View): void {
  if (typeof window === "undefined") return;
  const wanted = "#" + serializeView(next);
  if (window.location.hash !== wanted) {
    window.location.hash = wanted;
  }
}

// serializeView maps a view to its hash path. rowId / pageName
// flow through encodeURIComponent so non-ASCII ids + names with
// slashes survive the round trip.
function serializeView(v: View): string {
  switch (v.kind) {
    case "overview":
      return "/overview";
    case "list":
      return "/list/" + encodeURIComponent(v.pageName);
    case "detail":
      return "/detail/" + encodeURIComponent(v.pageName) + "/" + encodeURIComponent(v.rowId);
    case "create":
      return "/create/" + encodeURIComponent(v.pageName);
  }
}

// parseHash recovers the View from the location hash. Returns
// null when the hash is empty, unrecognized, or names a page
// the spec doesn't know — caller falls back to defaultView.
//
// Exported for unit tests: it is pure (hash + spec → view) and it
// decides whether a hand-typed URL reaches a form the page cannot
// submit, which is worth pinning without a full App render.
export function parseHash(hash: string, spec: AdminSpec): View | null {
  if (!hash || hash === "#" || hash === "#/") return null;
  // Strip leading "#" + leading "/" so segments line up.
  const path = hash.replace(/^#\/?/, "");
  const parts = path.split("/").map(decodeURIComponent);
  if (parts.length === 1 && parts[0] === "overview") {
    return overviewIsAvailable(spec) ? { kind: "overview" } : null;
  }
  if (parts.length === 2 && parts[0] === "list") {
    return spec.pages[parts[1]] ? { kind: "list", pageName: parts[1] } : null;
  }
  if (parts.length === 3 && parts[0] === "detail") {
    return spec.pages[parts[1]] ? { kind: "detail", pageName: parts[1], rowId: parts[2] } : null;
  }
  // Only route to a create form when the page actually declares one —
  // a hand-typed #/create/<page> on a read-only page falls back to the
  // default view rather than rendering a form that can't submit.
  if (parts.length === 2 && parts[0] === "create") {
    const p = spec.pages[parts[1]];
    return p?.detail?.create_endpoint ? { kind: "create", pageName: parts[1] } : null;
  }
  return null;
}

// identityOf picks a human label for the signed-in user to show
// in the header. The whoami shape is consumer-defined, so we
// probe the common identity fields in order and fall back to
// nothing (the header just omits the label) rather than showing
// a raw id.
function identityOf(whoami: WhoAmIResp | null): string | undefined {
  if (!whoami) return undefined;
  for (const key of ["email", "username", "name", "display_name"]) {
    const v = whoami[key];
    if (typeof v === "string" && v.trim()) return v;
  }
  return undefined;
}

// keep whoami accessible to future slot-based features without
// touching the App signature.
export type { WhoAmIResp };
