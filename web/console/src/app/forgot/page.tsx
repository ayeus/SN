"use client";

import Link from "next/link";
import { useState } from "react";
import { api, ApiError } from "@/lib/api";
import { AuthCard } from "@/components/AuthCard";
import { Button, Field, Input, Notice } from "@/components/ui";

export default function ForgotPage() {
  const [email, setEmail] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [sent, setSent] = useState<{ toLog: boolean } | null>(null);

  return (
    <AuthCard
      title="Reset your password"
      footer={
        <Link href="/login" className="font-medium text-nil hover:underline">
          Back to sign in
        </Link>
      }
    >
      {sent ? (
        <Notice tone="success">
          {sent.toLog ? (
            <>
              This is a development installation with no email server, so the reset link was written to{" "}
              <code className="font-mono text-[13px]">logs/control-api.log</code> instead of being emailed. Open the link from there.
            </>
          ) : (
            <>If {email} has an account, a reset link is on its way. It works once and expires in an hour.</>
          )}
        </Notice>
      ) : (
        <form
          className="grid gap-4"
          onSubmit={async (e) => {
            e.preventDefault();
            setBusy(true);
            setError("");
            try {
              const r = await api.post<{ delivered_to_log: boolean }>("/v1/auth/password/forgot", { email }, { auth: false });
              setSent({ toLog: r.delivered_to_log });
            } catch (err) {
              setError(err instanceof ApiError ? err.message : "Could not send the reset link");
            } finally {
              setBusy(false);
            }
          }}
        >
          {error && <Notice tone="error">{error}</Notice>}
          <Field label="Email" htmlFor="email" hint="We'll send a link to set a new password.">
            <Input id="email" type="email" autoComplete="email" required value={email} onChange={(e) => setEmail(e.target.value)} />
          </Field>
          <Button type="submit" loading={busy}>
            Send reset link
          </Button>
        </form>
      )}
    </AuthCard>
  );
}
