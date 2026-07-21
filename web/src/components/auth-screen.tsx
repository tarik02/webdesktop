import { AlertCircleIcon, LoaderCircleIcon, RefreshCwIcon } from "lucide-react";
import { useState, type FormEvent } from "react";
import { Alert, AlertDescription, AlertTitle } from "#/components/ui/alert.tsx";
import { Button } from "#/components/ui/button.tsx";
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "#/components/ui/card.tsx";
import { Field, FieldError, FieldGroup, FieldLabel } from "#/components/ui/field.tsx";
import { Input } from "#/components/ui/input.tsx";
import { apiErrorResponseSchema, authSessionSchema, type AuthSession } from "#/lib/protocol.ts";

type AuthScreenProps =
  | {
      phase: "login";
      session: AuthSession;
      onAuthenticated: (session: AuthSession) => void;
    }
  | {
      phase: "error";
      message: string;
      onRetry: () => void;
    };

export function AuthScreen(props: AuthScreenProps) {
  const [credential, setCredential] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (props.phase === "error") {
    return (
      <main className="flex min-h-svh items-center justify-center bg-muted p-4">
        <Card className="w-full max-w-sm">
          <CardHeader>
            <CardTitle>Webdesktop is unavailable</CardTitle>
            <CardDescription>The authentication service could not be reached.</CardDescription>
          </CardHeader>
          <CardContent>
            <Alert variant="destructive">
              <AlertCircleIcon />
              <AlertTitle>Connection failed</AlertTitle>
              <AlertDescription>{props.message}</AlertDescription>
            </Alert>
          </CardContent>
          <CardFooter>
            <Button className="w-full" onClick={props.onRetry}>
              <RefreshCwIcon data-icon="inline-start" />
              Retry
            </Button>
          </CardFooter>
        </Card>
      </main>
    );
  }

  const label =
    props.session.login_enabled && props.session.bearer_enabled
      ? "Password or bearer token"
      : props.session.login_enabled
        ? "Password"
        : "Bearer token";

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setSubmitting(true);
    setError(null);
    try {
      const response = await fetch("/api/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ credential }),
      });
      const body: unknown = await response.json();
      if (!response.ok) {
        const failure = apiErrorResponseSchema.safeParse(body);
        throw new Error(
          failure.success ? failure.data.error.message : `login returned ${response.status}`,
        );
      }
      const session = authSessionSchema.parse(body);
      if (!session.authenticated) {
        throw new Error("login did not create a browser session");
      }
      props.onAuthenticated(session);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "login failed");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <main className="flex min-h-svh items-center justify-center bg-muted p-4">
      <form className="w-full max-w-sm" onSubmit={(event) => void submit(event)}>
        <Card>
          <CardHeader>
            <CardTitle>Connect to this desktop</CardTitle>
            <CardDescription>
              {props.session.login_enabled && props.session.bearer_enabled
                ? "Enter the configured password or bearer token."
                : props.session.login_enabled
                  ? "Enter the configured webdesktop password."
                  : "Enter a configured webdesktop bearer token."}
            </CardDescription>
          </CardHeader>
          <CardContent>
            <FieldGroup>
              <Field data-invalid={error !== null}>
                <FieldLabel htmlFor="credential">{label}</FieldLabel>
                <Input
                  id="credential"
                  name="credential"
                  type="password"
                  autoComplete={props.session.login_enabled ? "current-password" : "off"}
                  autoFocus
                  required
                  maxLength={4096}
                  disabled={submitting}
                  aria-invalid={error !== null}
                  value={credential}
                  onChange={(event) => setCredential(event.currentTarget.value)}
                />
                {error ? <FieldError>{error}</FieldError> : null}
              </Field>
            </FieldGroup>
          </CardContent>
          <CardFooter>
            <Button className="w-full" type="submit" disabled={submitting}>
              {submitting ? (
                <LoaderCircleIcon className="animate-spin" data-icon="inline-start" />
              ) : null}
              Sign in
            </Button>
          </CardFooter>
        </Card>
      </form>
    </main>
  );
}
