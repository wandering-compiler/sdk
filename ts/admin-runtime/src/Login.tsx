// Login screen — branded, centered card.
//
// Two ways in, and a deployment picks which it offers:
//
//   - the username + password form, on unless
//     `auth.password_sign_in` is false;
//   - one button per `auth.sign_in_options`, each a link that starts a
//     federated flow somewhere else and comes back with the session in
//     the URL fragment (consumed in auth.ts::consumeRedirectToken).
//
// Both, either, in any number. The default — no options, password on —
// is the screen this was before federated sign-in existed.
//
// With an `auth.oauth_exchange_endpoint` a federated button is a PKCE start
// (oauth.ts): the link gains a `code_challenge` and the browser comes back
// with a one-time code instead of a session.
//
// With device identity on (`auth.dpop`) the password sign-in is a mint call
// (an unbound DPoP proof, no token) and the screen offers "This is a public
// computer": the session then gets the plugin's short public lifetime, its
// device is never trusted, and the token is kept in sessionStorage only.
//
// The color-scheme toggle is available here too so the OS-default theme
// can be overridden before signing in.

import { useState } from "react";
import {
  Alert,
  Button,
  Checkbox,
  Center,
  Container,
  Group,
  Paper,
  PasswordInput,
  Stack,
  Text,
  TextInput,
  Title,
} from "@mantine/core";

import { apiPost } from "./api";
import { setToken } from "./auth";
import { federatedStartURL } from "./oauth";
import { Brand, ThemeToggle } from "./components";
import { Divider } from "@mantine/core";
import { IconAlertTriangle } from "./icons";
import type { AdminSpec } from "./types";
import { useT } from "./i18n";

interface LoginResp {
  token: string;
  public_computer?: boolean;
}

export interface LoginProps {
  spec: AdminSpec;
  onLogin: () => void;
  /** Why the user is here again (a revoked device, a dropped sign-in link). */
  notice?: string;
}

export function Login({ spec, onLogin, notice }: LoginProps) {
  const t = useT();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const options = spec.auth.sign_in_options ?? [];
  // Absent means TRUE. A spec written before this field existed
  // describes a password admin, and reading the missing value as false
  // would blank its login page on the next regeneration.
  const passwordSignIn = spec.auth.password_sign_in !== false;
  const [error, setError] = useState<string | null>(null);
  const dpop = spec.auth.dpop === true;
  const pkce = !!spec.auth.oauth_exchange_endpoint;
  const [publicComputer, setPublicComputer] = useState(false);
  const [federatedError, setFederatedError] = useState<string | null>(null);
  // One start at a time: a second click would make a second verifier and
  // overwrite the first while the browser is already on its way.
  const [starting, setStarting] = useState(false);

  // A PKCE start is a button, not a link: the URL to go to exists only once
  // the challenge is made, and a link to the bare start_url (opened in a new
  // tab, copied) would reach the plugin without one and be refused.
  async function startFederated(startUrl: string) {
    setFederatedError(null);
    // No WebCrypto (a page not served over HTTPS): no challenge can be made.
    if (!(globalThis.crypto as Crypto | undefined)?.subtle) {
      setFederatedError(t("This browser cannot start a secure sign-in here — it needs HTTPS"));
      return;
    }
    setStarting(true);
    try {
      window.location.assign(
        await federatedStartURL(startUrl, { publicComputer: dpop && publicComputer }),
      );
    } catch (err) {
      setFederatedError(err instanceof Error ? err.message : String(err));
      setStarting(false);
    }
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    setSubmitting(true);
    try {
      const body: Record<string, unknown> = { username, password };
      // Only when ticked: a login method without the field refuses an
      // unknown one, and an unticked box has nothing to say.
      if (dpop && publicComputer) body.public_computer = true;
      // With device identity, a mint: an unbound proof and no token, re-sent
      // once on a fresh key when the stored one turns out to be revoked (a
      // password may be). Without it, the request it always was.
      const resp = await apiPost<LoginResp>(
        spec.auth.login_endpoint,
        body,
        undefined,
        dpop ? { mint: true, resign: true } : undefined,
      );
      if (!resp.token) {
        setError("login response missing 'token' field");
        return;
      }
      // The operator's own choice counts even when the answer cannot echo it
      // (a sign-in method without the field): a public computer must never
      // leave a token in the profile's storage.
      setToken(resp.token, {
        publicComputer: resp.public_computer === true || (dpop && publicComputer),
      });
      onLogin();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Center mih="100vh" p="md" pos="relative">
      <Group pos="absolute" top={16} right={16}>
        <ThemeToggle />
      </Group>
      <Container size={400} w="100%" px={0}>
        <Stack align="center" gap="lg">
          <Brand name={spec.name} size={40} />
          <Paper withBorder shadow="sm" p="xl" radius="lg" w="100%">
            <Stack gap="lg">
              <Stack gap={2}>
                <Title order={3}>{t("Sign in")}</Title>
                <Text c="dimmed" fz="sm">
                  {passwordSignIn
                    ? t("Enter your credentials to continue.")
                    : t("Continue with your organization account.")}
                </Text>
              </Stack>
              {notice && (
                <Alert color="yellow" variant="light" icon={<IconAlertTriangle size={18} />}>
                  {notice}
                </Alert>
              )}
              {options.length > 0 && (
                <Stack gap="sm">
                  {options.map((o) =>
                    pkce ? (
                      <Button
                        key={o.label}
                        onClick={() => startFederated(o.start_url)}
                        disabled={starting}
                        variant="default"
                        fullWidth
                        size="md"
                      >
                        {o.label}
                      </Button>
                    ) : (
                      <Button
                        key={o.label}
                        component="a"
                        href={o.start_url}
                        variant="default"
                        fullWidth
                        size="md"
                      >
                        {o.label}
                      </Button>
                    ),
                  )}
                  {federatedError && (
                    <Alert
                      color="red"
                      variant="light"
                      icon={<IconAlertTriangle size={18} />}
                      title={t("Sign-in failed")}
                    >
                      {federatedError}
                    </Alert>
                  )}
                </Stack>
              )}
              {options.length > 0 && passwordSignIn && (
                <Divider label={t("or")} labelPosition="center" />
              )}
              {passwordSignIn && (
                <form onSubmit={handleSubmit}>
                  <Stack>
                    <TextInput
                      label={t("Username")}
                      autoFocus
                      required
                      size="md"
                      value={username}
                      onChange={(e) => setUsername(e.currentTarget.value)}
                    />
                    <PasswordInput
                      label={t("Password")}
                      required
                      size="md"
                      value={password}
                      onChange={(e) => setPassword(e.currentTarget.value)}
                    />
                    {error && (
                      <Alert
                        color="red"
                        variant="light"
                        icon={<IconAlertTriangle size={18} />}
                        title={t("Sign-in failed")}
                      >
                        {error}
                      </Alert>
                    )}
                    <Button type="submit" loading={submitting} fullWidth size="md" mt="xs">
                      {t("Sign in")}
                    </Button>
                  </Stack>
                </form>
              )}
              {dpop && (
                <Checkbox
                  label={t("This is a public computer")}
                  description={t("Keep me signed in only until the browser closes.")}
                  checked={publicComputer}
                  onChange={(e) => setPublicComputer(e.currentTarget.checked)}
                />
              )}
            </Stack>
          </Paper>
        </Stack>
      </Container>
    </Center>
  );
}
