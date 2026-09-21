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
// The color-scheme toggle is available here too so the OS-default theme
// can be overridden before signing in.

import { useState } from "react";
import {
  Alert,
  Button,
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
import { Brand, ThemeToggle } from "./components";
import { Divider } from "@mantine/core";
import { IconAlertTriangle } from "./icons";
import type { AdminSpec } from "./types";
import { useT } from "./i18n";

interface LoginResp {
  token: string;
}

export interface LoginProps {
  spec: AdminSpec;
  onLogin: () => void;
}

export function Login({ spec, onLogin }: LoginProps) {
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

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    setSubmitting(true);
    try {
      const resp = await apiPost<LoginResp>(spec.auth.login_endpoint, {
        username,
        password,
      });
      if (!resp.token) {
        setError("login response missing 'token' field");
        return;
      }
      setToken(resp.token);
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
              {options.length > 0 && (
                <Stack gap="sm">
                  {options.map((o) => (
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
                  ))}
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
            </Stack>
          </Paper>
        </Stack>
      </Container>
    </Center>
  );
}
