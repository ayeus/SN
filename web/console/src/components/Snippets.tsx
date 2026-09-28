"use client";

import { useState } from "react";
import { CodeBlock, cx } from "./ui";

// PRD F-4: "Migration = one-line base_url change". These snippets are that line.
export function Snippets({ baseUrl, model, apiKey }: { baseUrl: string; model: string; apiKey?: string }) {
  const key = apiKey ?? "$API_KEY";
  const tabs = {
    curl: `curl ${baseUrl}/chat/completions \\
  -H "Authorization: Bearer ${key}" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${model}",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'`,
    Python: `from openai import OpenAI

client = OpenAI(base_url="${baseUrl}", api_key="${apiKey ?? "YOUR_API_KEY"}")

stream = client.chat.completions.create(
    model="${model}",
    messages=[{"role": "user", "content": "Hello!"}],
    stream=True,
)
for chunk in stream:
    print(chunk.choices[0].delta.content or "", end="")`,
    JavaScript: `import OpenAI from "openai";

const client = new OpenAI({ baseURL: "${baseUrl}", apiKey: "${apiKey ?? "YOUR_API_KEY"}" });

const stream = await client.chat.completions.create({
  model: "${model}",
  messages: [{ role: "user", content: "Hello!" }],
  stream: true,
});
for await (const chunk of stream) process.stdout.write(chunk.choices[0]?.delta?.content ?? "");`,
  };
  const [tab, setTab] = useState<keyof typeof tabs>("curl");

  return (
    <div>
      <div role="tablist" aria-label="Language" className="mb-2 flex gap-1">
        {(Object.keys(tabs) as (keyof typeof tabs)[]).map((t) => (
          <button
            key={t}
            role="tab"
            aria-selected={tab === t}
            onClick={() => setTab(t)}
            className={cx(
              "h-8 rounded-md px-3 text-[13px] font-medium",
              tab === t ? "bg-nil-soft text-nil" : "text-muted hover:bg-surface-2 hover:text-ink",
            )}
          >
            {t}
          </button>
        ))}
      </div>
      <CodeBlock code={tabs[tab]} />
    </div>
  );
}
