import Link from "next/link";
import { BRAND } from "@/lib/brand";

export function AuthCard({ title, children, footer }: { title: string; children: React.ReactNode; footer: React.ReactNode }) {
  return (
    <div className="grid min-h-screen place-items-center px-4 py-10">
      <div className="w-full max-w-[420px]">
        <Link href="/" className="mb-8 inline-block text-[17px] font-semibold tracking-[-0.01em]">
          {BRAND.name}
        </Link>
        <div className="rounded-xl border border-line bg-surface p-7">
          <h1 className="mb-6 text-[22px] font-semibold tracking-[-0.01em]">{title}</h1>
          {children}
        </div>
        <p className="mt-5 text-center text-[14px] text-muted">{footer}</p>
      </div>
    </div>
  );
}
