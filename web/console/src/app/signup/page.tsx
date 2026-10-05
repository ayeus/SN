"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useEffect, useState } from "react";
import { useAuth } from "@/lib/auth";
import { api, ApiError } from "@/lib/api";
import { AuthCard } from "@/components/AuthCard";
import { Button, Field, Input, Notice, Select, Skeleton } from "@/components/ui";

const COUNTRIES = [
  ["IN", "India"],
  ["SG", "Singapore"],
  ["AE", "United Arab Emirates"],
  ["GB", "United Kingdom"],
  ["DE", "Germany"],
  ["US", "United States"],
] as const;

// What the platform says about creating an account here.
type SignupInfo = {
  mode: "open" | "invite" | "closed";
  // True while the installation is still waiting for the person who runs it.
  owner_needed: boolean;
  invite?: { valid: boolean; problem?: string; email?: string | null; workspace?: string | null; role?: string };
};

const signInLink = (
  <>
    Already have an account?{" "}
    <Link href="/login" className="font-medium text-nil hover:underline">
      Sign in
    </Link>
  </>
);

function SignupForm() {
  const { signup } = useAuth();
  const router = useRouter();
  const params = useSearchParams();
  const host = params.get("intent") === "host";
  const inviteToken = params.get("invite") ?? "";
  const ownerParam = params.get("owner") ?? "";

  const [info, setInfo] = useState<SignupInfo | null>(null);
  const [form, setForm] = useState({ name: "", email: "", password: "", country: "IN", owner_code: ownerParam });
  const [fields, setFields] = useState<Record<string, string>>({});
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm((f) => ({ ...f, [k]: e.target.value }));

  useEffect(() => {
    let live = true;
    api
      .get<SignupInfo>(`/v1/auth/signup-info${inviteToken ? `?invite=${encodeURIComponent(inviteToken)}` : ""}`, false)
      .then((i) => {
        if (!live) return;
        setInfo(i);
        if (i.invite?.valid && i.invite.email) setForm((f) => ({ ...f, email: i.invite!.email! }));
      })
      // An older platform has no such endpoint: behave as it always did.
      .catch(() => live && setInfo({ mode: "open", owner_needed: false }));
    return () => {
      live = false;
    };
  }, [inviteToken]);

  if (!info) {
    return (
      <AuthCard title="Create your account" footer={signInLink}>
        <div className="grid gap-4">
          <Skeleton className="h-10" />
          <Skeleton className="h-10" />
          <Skeleton className="h-10" />
        </div>
      </AuthCard>
    );
  }

  const invite = inviteToken ? info.invite : undefined;
  // The owner sets the network up: either they followed the owner link, or
  // nobody has yet and there is no invitation in hand.
  const asOwner = !invite?.valid && (ownerParam !== "" || (info.owner_needed && info.mode !== "open"));
  const blocked = !asOwner && !invite?.valid && info.mode !== "open";

  if (blocked) {
    return (
      <AuthCard title={invite && !invite.valid ? "This invitation can't be used" : "Accounts are by invitation"} footer={signInLink}>
        <Notice tone={invite && !invite.valid ? "error" : "info"}>
          {invite && !invite.valid
            ? (invite.problem ?? "This invitation link is not valid. Ask for a new one.")
            : info.mode === "closed"
              ? "This network is not taking new accounts."
              : "This is a private network. Ask the person who runs it for an invitation link, then open that link to create your account."}
        </Notice>
      </AuthCard>
    );
  }

  const title = asOwner
    ? "Set up this network"
    : invite?.valid
      ? "You're invited"
      : host
        ? "Create an account to connect your GPU"
        : "Create your account";

  return (
    <AuthCard title={title} footer={signInLink}>
      <form
        className="grid gap-4"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError("");
          setFields({});
          try {
            await signup({
              name: form.name,
              email: form.email,
              password: form.password,
              country: form.country,
              ...(invite?.valid ? { invite: inviteToken } : {}),
              ...(asOwner ? { owner_code: form.owner_code.trim() } : {}),
            });
            router.replace(asOwner ? "/app/admin" : host ? "/app/hosts/new" : "/app/models");
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
        {inviteToken && invite && !invite.valid && info.mode === "open" && (
          <Notice tone="warn">{invite.problem ?? "This invitation link is not valid."} You can still create an account of your own.</Notice>
        )}
        {asOwner && (
          <Notice>
            The first account runs this network: it invites people and sees every machine. It needs the owner code from the
            installation&apos;s <code className="font-mono text-[13px]">private.env</code> file.
          </Notice>
        )}
        {invite?.valid && (
          <Notice>
            {invite.workspace
              ? `You'll join ${invite.workspace} and share its deployments and API keys.`
              : "You'll get a workspace of your own on this network."}
          </Notice>
        )}
        {asOwner && (
          <Field label="Owner code" htmlFor="owner_code">
            <Input id="owner_code" autoComplete="off" spellCheck={false} required value={form.owner_code} onChange={set("owner_code")} className="font-mono" />
          </Field>
        )}
        <Field label="Your name" htmlFor="name" error={fields.name}>
          <Input id="name" autoComplete="name" required value={form.name} onChange={set("name")} aria-invalid={!!fields.name} />
        </Field>
        <Field
          label="Email"
          htmlFor="email"
          error={fields.email}
          hint={invite?.valid && invite.email ? "This invitation is for this address." : undefined}
        >
          <Input
            id="email"
            type="email"
            autoComplete="email"
            required
            readOnly={!!(invite?.valid && invite.email)}
            value={form.email}
            onChange={set("email")}
            aria-invalid={!!fields.email}
          />
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
        {!invite?.workspace && (
          <Field label="Country" htmlFor="country" hint="Sets the region your deployments start in. You can change it per deployment.">
            <Select id="country" value={form.country} onChange={set("country")}>
              {COUNTRIES.map(([code, name]) => (
                <option key={code} value={code}>
                  {name}
                </option>
              ))}
            </Select>
          </Field>
        )}
        <Button type="submit" loading={busy} className="mt-2">
          {asOwner ? "Create the owner account" : "Create account"}
        </Button>
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
