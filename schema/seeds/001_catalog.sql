-- AyeusANN Seed Data: Catalogue models (PRD F-2, prices PRD §9)
-- Run after all migrations. Idempotent: re-running updates the rows in place.
--
-- This file seeds catalogue metadata only. It deliberately does NOT seed a
-- model_artifacts row: an artifact record asserts "these exact bytes, with this
-- digest, live at this URL", and there is nothing truthful to put in the
-- checksum column until the weights have actually been fetched and hashed.
--
-- runtime_refs tells the coordinator what to ask each host runtime for:
--   ollama : 4-bit community builds, used on T3 laptops, Apple Silicon and dev
--   vllm   : full-precision Hugging Face weights, used on T1/T2 NVIDIA nodes
-- min_vram_gb inside a ref is the floor for that build; the model-level
-- min_vram_gb is the full-precision floor.
--
-- Prices are USD per 1M tokens, the on-demand (T1/T2) rate. T3 spot is billed at
-- a discount applied by the billing package (PRD: 50–60% of on-demand).

INSERT INTO models (
    id, name, family, params_b, license, min_vram_gb, tiers_allowed,
    price_in_per_1m, price_out_per_1m, price_per_hour_inr, is_byo,
    quantization_presets, runtime_refs, description, context_length
) VALUES
(
    '550e8400-e29b-41d4-a716-446655440001', 'llama-3.1-8b-instruct', 'llama', 8.0,
    'Llama 3.1 Community License', 16, '{t1,t2,t3}', 0.0800, 0.2200, 29.00, FALSE,
    '[{"name":"best","quantization":"none","min_vram_gb":18,"description":"Full precision BF16 on vLLM"},
      {"name":"balanced","quantization":"q4_K_M","min_vram_gb":8,"description":"4-bit build, runs on laptops and Apple Silicon"}]'::jsonb,
    '{"ollama":{"model":"llama3.1:8b","quantization":"q4_K_M","min_vram_gb":8},
      "vllm":{"model":"meta-llama/Llama-3.1-8B-Instruct","quantization":"bf16","min_vram_gb":18}}'::jsonb,
    'General-purpose 8B chat model. The M1 reference model.', 131072
),
(
    '550e8400-e29b-41d4-a716-446655440011', 'qwen2.5-7b-instruct', 'qwen', 7.6,
    'Apache-2.0', 16, '{t1,t2,t3}', 0.0700, 0.2000, 29.00, FALSE,
    '[{"name":"best","quantization":"none","min_vram_gb":18,"description":"Full precision BF16 on vLLM"},
      {"name":"balanced","quantization":"q4_K_M","min_vram_gb":8,"description":"4-bit build, runs on laptops and Apple Silicon"}]'::jsonb,
    '{"ollama":{"model":"qwen2.5:7b","quantization":"q4_K_M","min_vram_gb":8},
      "vllm":{"model":"Qwen/Qwen2.5-7B-Instruct","quantization":"bf16","min_vram_gb":18}}'::jsonb,
    'Strong multilingual 7B chat model with an Apache-2.0 licence.', 32768
),
(
    '550e8400-e29b-41d4-a716-446655440012', 'mistral-7b-instruct-v0.3', 'mistral', 7.2,
    'Apache-2.0', 16, '{t1,t2,t3}', 0.0700, 0.1900, 29.00, FALSE,
    '[{"name":"best","quantization":"none","min_vram_gb":18,"description":"Full precision BF16 on vLLM"},
      {"name":"balanced","quantization":"q4_K_M","min_vram_gb":8,"description":"4-bit build"}]'::jsonb,
    '{"ollama":{"model":"mistral:7b","quantization":"q4_K_M","min_vram_gb":8},
      "vllm":{"model":"mistralai/Mistral-7B-Instruct-v0.3","quantization":"bf16","min_vram_gb":18}}'::jsonb,
    'Fast 7B instruction model, good for agents and structured output.', 32768
),
(
    '550e8400-e29b-41d4-a716-446655440014', 'qwen2.5-coder-7b-instruct', 'qwen', 7.6,
    'Apache-2.0', 16, '{t1,t2,t3}', 0.0800, 0.2200, 29.00, FALSE,
    '[{"name":"best","quantization":"none","min_vram_gb":18,"description":"Full precision BF16 on vLLM"},
      {"name":"balanced","quantization":"q4_K_M","min_vram_gb":8,"description":"4-bit build"}]'::jsonb,
    '{"ollama":{"model":"qwen2.5-coder:7b","quantization":"q4_K_M","min_vram_gb":8},
      "vllm":{"model":"Qwen/Qwen2.5-Coder-7B-Instruct","quantization":"bf16","min_vram_gb":18}}'::jsonb,
    'Code generation and completion model (PRD F-2 "one code model").', 32768
),
(
    '550e8400-e29b-41d4-a716-446655440015', 'gemma-2-2b-it', 'gemma', 2.6,
    'Gemma Terms of Use', 6, '{t1,t2,t3}', 0.0300, 0.0800, 11.00, FALSE,
    '[{"name":"best","quantization":"none","min_vram_gb":6,"description":"Full precision on vLLM"},
      {"name":"balanced","quantization":"q4_0","min_vram_gb":3,"description":"4-bit build for small laptops"}]'::jsonb,
    '{"ollama":{"model":"gemma2:2b","quantization":"q4_0","min_vram_gb":3},
      "vllm":{"model":"google/gemma-2-2b-it","quantization":"bf16","min_vram_gb":6}}'::jsonb,
    'Small, cheap model suited to T3 spot capacity and batch jobs.', 8192
),
(
    '550e8400-e29b-41d4-a716-446655440013', 'llama-3.3-70b-instruct', 'llama', 70.6,
    'Llama 3.3 Community License', 140, '{t1,t2}', 0.5500, 1.6000, 117.00, FALSE,
    '[{"name":"best","quantization":"none","min_vram_gb":140,"description":"Full precision, 2x80GB"},
      {"name":"balanced","quantization":"q4_K_M","min_vram_gb":48,"description":"4-bit, single 48GB card"}]'::jsonb,
    '{"ollama":{"model":"llama3.3:70b","quantization":"q4_K_M","min_vram_gb":48},
      "vllm":{"model":"meta-llama/Llama-3.3-70B-Instruct","quantization":"bf16","min_vram_gb":140}}'::jsonb,
    '70B-class flagship. Tier 1/2 only; too large for personal machines.', 131072
)
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    family = EXCLUDED.family,
    params_b = EXCLUDED.params_b,
    license = EXCLUDED.license,
    min_vram_gb = EXCLUDED.min_vram_gb,
    tiers_allowed = EXCLUDED.tiers_allowed,
    price_in_per_1m = EXCLUDED.price_in_per_1m,
    price_out_per_1m = EXCLUDED.price_out_per_1m,
    price_per_hour_inr = EXCLUDED.price_per_hour_inr,
    quantization_presets = EXCLUDED.quantization_presets,
    runtime_refs = EXCLUDED.runtime_refs,
    description = EXCLUDED.description,
    context_length = EXCLUDED.context_length,
    updated_at = NOW();

-- Partner price book (PRD F-8 / SRS FR-75): incubator portfolio = cost + thin margin.
INSERT INTO price_books (id, name, description) VALUES (
    '550e8400-e29b-41d4-a716-446655440003',
    'partner-rate',
    'Incubator portfolio partner rate: cost + thin margin'
) ON CONFLICT DO NOTHING;

INSERT INTO price_book_entries (price_book_id, gpu_model, tier, price_per_hour, price_in_per_1m, price_out_per_1m) VALUES
    ('550e8400-e29b-41d4-a716-446655440003', 'RTX 4090', 't1', 18.00, 0.0500, 0.1400),
    ('550e8400-e29b-41d4-a716-446655440003', 'RTX 5090', 't1', 26.00, 0.0600, 0.1600)
ON CONFLICT DO NOTHING;
