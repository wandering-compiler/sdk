// Login screen — branded, centered card. Username + password;
// SSO / 2FA parked. The color-scheme toggle is available here too
// so the OS-default theme can be overridden before signing in.

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
import { IconAlertTriangle } from "./icons";
import type { AdminSpec } from "./types";

interface LoginResp {
  token: string;
}

export interface LoginProps {
  spec: AdminSpec;
  onLogin: () => void;
}

export function Login({ spec, onLogin }: LoginProps) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
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
                <Title order={3}>Sign in</Title>
                <Text c="dimmed" fz="sm">
                  Enter your credentials to continue.
                </Text>
              </Stack>
              <form onSubmit={handleSubmit}>
                <Stack>
                  <TextInput
                    label="Username"
                    autoFocus
                    required
                    size="md"
                    value={username}
                    onChange={(e) => setUsername(e.currentTarget.value)}
                  />
                  <PasswordInput
                    label="Password"
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
                      title="Sign-in failed"
                    >
                      {error}
                    </Alert>
                  )}
                  <Button type="submit" loading={submitting} fullWidth size="md" mt="xs">
                    Sign in
                  </Button>
                </Stack>
              </form>
            </Stack>
          </Paper>
        </Stack>
      </Container>
    </Center>
  );
}
