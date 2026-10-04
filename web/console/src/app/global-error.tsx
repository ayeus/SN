"use client";

// Last-resort boundary for errors in the root layout itself.
export default function GlobalError({ error, reset }: { error: Error & { digest?: string }; reset: () => void }) {
  return (
    <html lang="en">
      <body style={{ fontFamily: "system-ui, sans-serif", padding: 32, maxWidth: 640 }}>
        <h1 style={{ fontSize: 22 }}>Something went wrong</h1>
        <p style={{ color: "#5d6279" }}>{error.message || "The page failed to load."}</p>
        {error.digest && <p style={{ color: "#5d6279", fontSize: 12 }}>Reference: {error.digest}</p>}
        <button onClick={reset} style={{ marginTop: 16, padding: "8px 16px" }}>
          Try again
        </button>
      </body>
    </html>
  );
}
