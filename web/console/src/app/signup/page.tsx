"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useState } from "react";
import { useAuth } from "@/lib/auth";
import { ApiError } from "@/lib/api";
import { AuthCard } from "@/components/AuthCard";
import { Button, Field, Input, Notice, Select } from "@/components/ui";

const COUNTRIES = [
  ["IN", "India"],
  ["SG", "Singapore"],
  ["AE", "United Arab Emirates"],
  ["GB", "United Kingdom"],
  ["DE", "Germany"],
  ["US", "United States"],
] as const;

function SignupForm() {
  const { signup } = useAuth();
  const router = useRouter();
  const host = useSearchParams().get("intent") === "host";
  const [form, setForm] = useState({ name: "", email: "", password: "", country: "IN" });
  const [fields, setFields] = useState<Record<string, string>>({});
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm((f) => ({ ...f, [k]: e.target.value }));

  return (
    <AuthCard
      title={host ? "Create an account to connect your GPU" : "Create your account"}
      footer={
        <>
          Already have an account?{" "}
          <Link href="/login" className="font-medium text-nil hover:underline">
            Sign in
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
          setFields({});
          try {
            const credit = await signup(form);
            const dest = host ? "/app/hosts/new" : "/app/models";
            router.replace(credit ? `${dest}?welcome=${encodeURIComponent(credit)}` : dest);
          } catch (err) {
            if (err instanceof ApiError) {
              setFields(err.fields);
              if (!Object.keys(err.fields).length) setError(err.message);
            } else setError("Sign-up failed");
          } finally {
            setBusy(false);
          }
        }}
      >
        {error && <Notice tone="error">{error}</Notice>}
        <Field label="Your name" htmlFor="name" error={fields.name}>
          <Input id="name" autoComplete="name" required value={form.name} onChange={set("name")} aria-invalid={!!fields.name} />
        </Field>
        <Field label="Work email" htmlFor="email" error={fields.email}>
          <Input id="email" type="email" autoComplete="email" required value={form.email} onChange={set("email")} aria-invalid={!!fields.email} />
        </Field>
        <Field
          label="Password"
          htmlFor="password"
          error={fields.password}
          hint="At least 12 characters, using three of: lowercase, uppercase, digits, symbols."
        >
          <Input
            id="password"
            type="password"
            autoComplete="new-password"
            required
            minLength={12}
            value={form.password}
            onChange={set("password")}
            aria-invalid={!!fields.password}
          />
        </Field>
        <Field label="Billing country" htmlFor="country" hint="Sets your wallet currency and nearest region. India bills in rupees with GST.">
          <Select id="country" value={form.country} onChange={set("country")}>
            {COUNTRIES.map(([code, name]) => (
              <option key={code} value={code}>
                {name}
              </option>
            ))}
          </Select>
        </Field>
        <Button type="submit" loading={busy} className="mt-2">
          Create account
        </Button>
        <p className="text-[13px] text-muted">New accounts in India get ₹500 of credit. No card needed.</p>
      </form>
    </AuthCard>
  );
}

export default function SignupPage() {
  return (
    <Suspense>
      <SignupForm />
    </Suspense>
  );
}
