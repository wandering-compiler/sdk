// AdminSwitcher — the project's other admins, beside the heading: a domain's
// admin links to another domain's (`AdminApi.links`). The addresses are the
// environment's, not the build's: the admin server answers them from its env
// (`<PREFIX>_LINK_<DOMAIN>`), so the same bundle links to localhost on a
// developer's machine and to the public hosts when deployed. A link the
// environment gives no address is not offered.
import { useEffect, useState } from "react";
import { Button, Menu } from "@mantine/core";
import { IconChevronDown } from "../icons";

export interface AdminSwitcherLink {
  domain: string;
  label: string;
}

export interface AdminSwitcherProps {
  links: AdminSwitcherLink[];
  /** GET answering `{"links": {"<domain>": "<url>", …}}`. */
  endpoint: string;
  t?: (s: string) => string;
}

export function AdminSwitcher({ links, endpoint, t = (s) => s }: AdminSwitcherProps) {
  const [urls, setUrls] = useState<Record<string, string> | null>(null);
  useEffect(() => {
    let live = true;
    fetch(endpoint, { headers: { Accept: "application/json" } })
      .then((r) => (r.ok ? r.json() : { links: {} }))
      .then((body: { links?: Record<string, string> }) => {
        if (live) setUrls(body.links ?? {});
      })
      .catch(() => {
        if (live) setUrls({});
      });
    return () => {
      live = false;
    };
  }, [endpoint]);
  const reachable = links.filter((l) => urls?.[l.domain]);
  if (reachable.length === 0) return null;
  return (
    <Menu position="bottom-start" withinPortal>
      <Menu.Target>
        <Button
          variant="subtle"
          size="compact-sm"
          rightSection={<IconChevronDown size={14} />}
          aria-label={t("Switch admin")}
        >
          {t("Switch admin")}
        </Button>
      </Menu.Target>
      <Menu.Dropdown>
        {reachable.map((l) => (
          <Menu.Item key={l.domain} component="a" href={urls![l.domain]}>
            {t(l.label)}
          </Menu.Item>
        ))}
      </Menu.Dropdown>
    </Menu>
  );
}
