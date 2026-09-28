"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useState } from "react";
import { useAuth } from "@/lib/auth";
import { ApiError } from "@/lib/api";
import { AuthCard } from "@/components/AuthCard";
import { Button, Field, Input, Notice } from "@/components/ui";

function LoginForm() {
  const { login } = useAuth();
  const router = useRouter();
  const next = useSearchParams().get("next") || "/app";
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  return (
    <AuthCard
      title="Sign in"
      footer={
        <>
          New here?{" "}
          <Link href="/signup" className="font-medium text-nil hover:underline">
            Create an account
          </Link>
        </>
      }
    >
      <form
        className="grid gap-4"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError("");
          try {
            await login(email, password);
            router.replace(next.startsWith("/") ? next : "/app");
          } catch (err) {
            setError(err instanceof ApiError ? err.message : "Sign-in failed");
          } finally {
            setBusy(false);
          }
        }}
      >
        {error && <Notice tone="error">{error}</Notice>}
        <Field label="Email" htmlFor="email">
          <Input id="email" type="email" autoComplete="email" required value={email} onChange={(e) => setEmail(e.target.value)} />
        </Field>
        <Field label="Password" htmlFor="password">
          <Input
            id="password"
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </Field>
        <Button type="submit" loading={busy} className="mt-2">
          Sign in
        </Button>
      </form>
    </AuthCard>
  );
}

export default function LoginPage() {
  return (
    <Suspense>
      <LoginForm />
    </Suspense>
  );
}
