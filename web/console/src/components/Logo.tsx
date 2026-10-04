import { BRAND } from "@/lib/brand";

// The mark is the product's own signature: the five-stage power rail, ending
// on the marigold "live" stage.
export function LogoMark({ size = 28 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 28 28" aria-hidden className="shrink-0">
      <rect width="28" height="28" rx="8" fill="var(--nil)" />
      <rect x="6" y="8" width="16" height="2.6" rx="1.3" fill="var(--on-nil)" opacity="0.45" />
      <rect x="6" y="12.7" width="16" height="2.6" rx="1.3" fill="var(--on-nil)" opacity="0.75" />
      <rect x="6" y="17.4" width="9" height="2.6" rx="1.3" fill="var(--on-nil)" />
      <rect x="16.5" y="17.4" width="5.5" height="2.6" rx="1.3" fill="var(--marigold)" />
    </svg>
  );
}

export function Logo({ size = 28 }: { size?: number }) {
  return (
    <span className="inline-flex items-center gap-2.5 text-[17px] font-semibold tracking-[-0.015em]">
      <LogoMark size={size} />
      {BRAND.name}
    </span>
  );
}
