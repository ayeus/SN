import Link from "next/link";

export default function NotFound() {
  return (
    <div className="grid min-h-screen place-items-center px-4">
      <div className="max-w-[440px]">
        <h1 className="text-[22px] font-semibold">Page not found</h1>
        <p className="mt-2 text-muted">The address may be mistyped, or the page has moved.</p>
        <div className="mt-6 flex gap-4 text-[14px]">
          <Link href="/app" className="font-medium text-nil hover:underline">
            Open the console
          </Link>
          <Link href="/" className="text-muted hover:text-ink">
            Home
          </Link>
        </div>
      </div>
    </div>
  );
}
