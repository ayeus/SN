"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useState } from "react";
import { api, ApiError } from "@/lib/api";
import { AuthCard } from "@/components/AuthCard";
import { Button, Field, Input, Notice } from "@/components/ui";

function ResetForm() {
  const token = useSearchParams().get("token") ?? "";
  const router = useRouter();
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [fieldError, setFieldError] = useState("");

  return (
    <AuthCard
      title="Choose a new password"
      footer={
        <Link href="/forgot" className="font-medium text-nil hover:underline">
          Request a new link
        </Link>
      }
    >
      {!token ? (
        <Notice tone="error">This link is missing its token. Request a new reset link.</Notice>
      ) : (
        <form
          className="grid gap-4"
          onSubmit={async (e) => {
            e.preventDefault();
            setBusy(true);
            setError("");
            setFieldError("");
            try {
              await api.post("/v1/auth/password/reset", { token, password }, { auth: false });
              router.replace("/login?reset=1");
            } catch (err) {
              if (err instanceof ApiError && err.fields.password) setFieldError(err.fields.password);
              else setError(err instanceof ApiError ? err.message : "Could not reset the password");
            } finally {
              setBusy(false);
            }
          }}
        >
          {error && <Notice tone="error">{error}</Notice>}
          <Field
            label="New password"
            htmlFor="password"
            error={fieldError}
            hint="At least 12 characters, using three of: lowercase, uppercase, digits, symbols."
          >
            <Input id="password" type="password" autoComplete="new-password" required minLength={12} value={password} onChange={(e) => setPassword(e.target.value)} />
          </Field>
          <Button type="submit" loading={busy}>
            Set new password
          </Button>
          <p className="text-[13px] text-muted">Setting a new password signs you out on every device.</p>
        </form>
      )}
    </AuthCard>
  );
}

export default function ResetPage() {
  return (
    <Suspense>
      <ResetForm />
    </Suspense>
  );
}
