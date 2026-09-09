package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/ayeus/ayeusann/internal/auth"
	"github.com/ayeus/ayeusann/internal/db"
)

// ─── OpenAI-compatible Types ──────────────────────────────────

type ChatCompletionRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatCompletionResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	SystemFingerprint string   `json:"system_fingerprint"`
	Choices           []Choice `json:"choices"`
	Usage             Usage    `json:"usage"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamChunk represents an SSE streaming chunk (OpenAI delta format).
type StreamChunk struct {
	ID                string         `json:"id"`
	Object            string         `json:"object"`
	Created           int64          `json:"created"`
	Model             string         `json:"model"`
	SystemFingerprint string         `json:"system_fingerprint"`
	Choices           []StreamChoice `json:"choices"`
}

type StreamChoice struct {
	Index        int          `json:"index"`
	Delta        StreamDelta  `json:"delta"`
	FinishReason *string      `json:"finish_reason"`
}

type StreamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// APIKeyInfo holds resolved API key metadata.
type APIKeyInfo struct {
	ID     string
	OrgID  string
	Scope  string
}

// InferenceHandler handles OpenAI-compatible inference requests.
type InferenceHandler struct {
	db          *db.Client
	rateLimiter *RateLimiter
	routerURL   string
	billingURL  string
	svcAuth     *auth.ServiceAuthenticator
	httpClient  *http.Client
}

func NewInferenceHandler(database *db.Client, rl *RateLimiter, routerURL, billingURL string, svcAuth *auth.ServiceAuthenticator) *InferenceHandler {
	return &InferenceHandler{
		db:          database,
		rateLimiter: rl,
		routerURL:   routerURL,
		billingURL:  billingURL,
		svcAuth:     svcAuth,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// HandleChatCompletions processes OpenAI-compatible /v1/chat/completions requests.
func (h *InferenceHandler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key, bypass-tunnel-reminder")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	// 1. Authenticate API key
	apiKey, err := h.authenticateAPIKey(r)
	if err != nil {
		writeGatewayError(w, http.StatusUnauthorized, "authentication_error", err.Error())
		return
	}

	// 2. Rate limit check
	if h.rateLimiter != nil {
		result, rlErr := h.rateLimiter.Check(r.Context(), apiKey.ID)
		if rlErr == nil {
			h.rateLimiter.WriteRateLimitHeaders(w, result)
			if !result.Allowed {
				writeGatewayError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
					"Rate limit exceeded. Please retry after the Retry-After period.")
				return
			}
		}
		// If Redis is down, we allow the request through (fail-open)
	}

	// 3. Parse request body
	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeGatewayError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON payload")
		return
	}

	if req.Model == "" {
		req.Model = "llama-3.1-8b-instruct"
	}
	if len(req.Messages) == 0 {
		writeGatewayError(w, http.StatusBadRequest, "invalid_request_error", "Messages array cannot be empty")
		return
	}

	// 4. Validate model exists
	modelExists, err := h.validateModel(r.Context(), req.Model)
	if err != nil || !modelExists {
		writeGatewayError(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("Model '%s' not found in the AyeusANN catalog", req.Model))
		return
	}

	// 5. Check wallet balance (warn if low, but don't block)
	balance, _ := h.getOrgBalance(r.Context(), apiKey.OrgID)
	if balance <= 0 {
		writeGatewayError(w, http.StatusPaymentRequired, "insufficient_balance",
			"Insufficient wallet balance. Please top up your account.")
		return
	}

	// 6. Generate request ID for tracing and idempotency
	requestID := uuid.New().String()

	// 7. Route to a replica via the Router service
	routeResp, err := h.routeRequest(r.Context(), req, apiKey.OrgID, requestID)
	if err != nil {
		writeGatewayError(w, http.StatusBadGateway, "routing_error",
			fmt.Sprintf("Failed to route request to GPU replica: %v", err))
		return
	}

	// 8. Respond to client
	if req.Stream {
		h.writeSSEStream(w, routeResp, req.Model, requestID)
	} else {
		h.writeNonStreamResponse(w, routeResp, req.Model, requestID)
	}

	// 9. Emit usage event to billing meter (fire-and-forget)
	go h.emitUsageEvent(requestID, routeResp, req, apiKey.OrgID)
}

// HandleListModels returns available models for the authenticated org.
func (h *InferenceHandler) HandleListModels(w http.ResponseWriter, r *http.Request) {
	apiKey, err := h.authenticateAPIKey(r)
	if err != nil {
		writeGatewayError(w, http.StatusUnauthorized, "authentication_error", err.Error())
		return
	}

	query := `
		SELECT m.name, m.family, m.params_b, m.min_vram_gb, m.license
		FROM models m
		JOIN deployments d ON d.model_id = m.id
		WHERE d.org_id = $1
		  AND d.state = 'serving'
		  AND d.deleted_at IS NULL
		GROUP BY m.id, m.name, m.family, m.params_b, m.min_vram_gb, m.license
		ORDER BY m.name;
	`

	rows, err := h.db.Pool.Query(r.Context(), query, apiKey.OrgID)
	if err != nil {
		writeGatewayError(w, http.StatusInternalServerError, "internal_error", "Failed to list models")
		return
	}
	defer rows.Close()

	type modelInfo struct {
		ID      string  `json:"id"`
		Object  string  `json:"object"`
		OwnedBy string  `json:"owned_by"`
		Family  string  `json:"family"`
		ParamsB float32 `json:"params_b"`
		MinVRAM int     `json:"min_vram_gb"`
		License string  `json:"license"`
	}

	var models []modelInfo
	for rows.Next() {
		var m modelInfo
		if err := rows.Scan(&m.ID, &m.Family, &m.ParamsB, &m.MinVRAM, &m.License); err != nil {
			continue
		}
		m.Object = "model"
		m.OwnedBy = "ayeusann"
		models = append(models, m)
	}

	// If no serving deployments, return catalog models
	if len(models) == 0 {
		catalogQuery := `
			SELECT name, family, params_b, min_vram_gb, license
			FROM models
			ORDER BY name;
		`
		catRows, err := h.db.Pool.Query(r.Context(), catalogQuery)
		if err == nil {
			defer catRows.Close()
			for catRows.Next() {
				var m modelInfo
				if err := catRows.Scan(&m.ID, &m.Family, &m.ParamsB, &m.MinVRAM, &m.License); err != nil {
					continue
				}
				m.Object = "model"
				m.OwnedBy = "ayeusann"
				models = append(models, m)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
}

// ─── Authentication ───────────────────────────────────────────

func (h *InferenceHandler) authenticateAPIKey(r *http.Request) (*APIKeyInfo, error) {
	// Try X-API-Key header first, then Authorization: Bearer sk_live_*
	rawKey := r.Header.Get("X-API-Key")
	if rawKey == "" {
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
				rawKey = parts[1]
			}
		}
	}

	if rawKey == "" {
		return nil, fmt.Errorf("missing API key (set X-API-Key header or Authorization: Bearer sk_live_...)")
	}

	// Extract prefix for fast DB lookup
	prefix, err := auth.ExtractPrefix(rawKey)
	if err != nil {
		return nil, fmt.Errorf("invalid API key format")
	}

	// Look up by prefix, verify hash
	hash := auth.HashAPIKey(rawKey)

	var keyInfo APIKeyInfo
	query := `
		SELECT id, org_id, scope
		FROM api_keys
		WHERE prefix = $1 AND hash = $2 AND NOT revoked;
	`
	err = h.db.Pool.QueryRow(r.Context(), query, prefix, hash).Scan(
		&keyInfo.ID, &keyInfo.OrgID, &keyInfo.Scope,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("invalid or revoked API key")
		}
		return nil, fmt.Errorf("authentication error")
	}

	// Update last_used_at (fire-and-forget)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = h.db.Pool.Exec(ctx, "UPDATE api_keys SET last_used_at = NOW() WHERE id = $1", keyInfo.ID)
	}()

	return &keyInfo, nil
}

// ─── Model Validation ─────────────────────────────────────────

func (h *InferenceHandler) validateModel(ctx context.Context, modelName string) (bool, error) {
	var exists bool
	err := h.db.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM models WHERE name = $1)", modelName).Scan(&exists)
	return exists, err
}

// ─── Balance Check ────────────────────────────────────────────

func (h *InferenceHandler) getOrgBalance(ctx context.Context, orgID string) (float64, error) {
	var balance float64
	query := `
		SELECT COALESCE(balance_after, 0)
		FROM wallet_ledger
		WHERE org_id = $1
		ORDER BY created_at DESC, entry_id DESC
		LIMIT 1;
	`
	err := h.db.Pool.QueryRow(ctx, query, orgID).Scan(&balance)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return balance, nil
}

// ─── Router Proxy ─────────────────────────────────────────────

type routeRequestBody struct {
	DeploymentID string          `json:"deployment_id"`
	Model        string          `json:"model"`
	OrgID        string          `json:"org_id"`
	Body         json.RawMessage `json:"body"`
	Stream       bool            `json:"stream"`
}

type routeResponseBody struct {
	ReplicaID  string `json:"replica_id"`
	HostID     string `json:"host_id"`
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

func (h *InferenceHandler) routeRequest(ctx context.Context, req ChatCompletionRequest, orgID, requestID string) (*routeResponseBody, error) {
	reqBody, _ := json.Marshal(req)

	routeReq := routeRequestBody{
		Model: req.Model,
		OrgID: orgID,
		Body:  reqBody,
		Stream: req.Stream,
	}

	payload, _ := json.Marshal(routeReq)
	url := h.routerURL + "/v1/route"

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("gateway: failed to create route request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Request-ID", requestID)
	if h.svcAuth != nil {
		h.svcAuth.SignRequest(httpReq)
	}

	resp, err := h.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gateway: router unreachable: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("gateway: router returned %d: %s", resp.StatusCode, string(body))
	}

	var routeResp routeResponseBody
	if err := json.Unmarshal(body, &routeResp); err != nil {
		return nil, fmt.Errorf("gateway: failed to decode router response: %w", err)
	}

	return &routeResp, nil
}

// ─── Response Writers ─────────────────────────────────────────

func (h *InferenceHandler) writeNonStreamResponse(w http.ResponseWriter, routeResp *routeResponseBody, model, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)

	if routeResp.Body != "" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(routeResp.Body))
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"error":"empty response from replica"}`))
}

func (h *InferenceHandler) writeSSEStream(w http.ResponseWriter, routeResp *routeResponseBody, model, requestID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeGatewayError(w, http.StatusInternalServerError, "internal_error", "Streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)

	// Parse the complete response and stream it as SSE chunks
	var fullResp ChatCompletionResponse
	if err := json.Unmarshal([]byte(routeResp.Body), &fullResp); err != nil {
		// Can't parse, send as single chunk
		chunk := StreamChunk{
			ID:                "chatcmpl-" + requestID[:12],
			Object:            "chat.completion.chunk",
			Created:           time.Now().Unix(),
			Model:             model,
			SystemFingerprint: "fp_AyeusANN_gpu",
			Choices: []StreamChoice{
				{Index: 0, Delta: StreamDelta{Content: routeResp.Body}, FinishReason: strPtr("stop")},
			},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	content := ""
	if len(fullResp.Choices) > 0 {
		content = fullResp.Choices[0].Message.Content
	}

	streamID := fullResp.ID
	if streamID == "" {
		streamID = "chatcmpl-" + requestID[:12]
	}

	// Send role chunk
	roleChunk := StreamChunk{
		ID:                streamID,
		Object:            "chat.completion.chunk",
		Created:           fullResp.Created,
		Model:             model,
		SystemFingerprint: fullResp.SystemFingerprint,
		Choices: []StreamChoice{
			{Index: 0, Delta: StreamDelta{Role: "assistant"}},
		},
	}
	data, _ := json.Marshal(roleChunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()

	// Stream content in word-sized chunks
	words := strings.Fields(content)
	for i, word := range words {
		prefix := ""
		if i > 0 {
			prefix = " "
		}

		chunk := StreamChunk{
			ID:                streamID,
			Object:            "chat.completion.chunk",
			Created:           fullResp.Created,
			Model:             model,
			SystemFingerprint: fullResp.SystemFingerprint,
			Choices: []StreamChoice{
				{Index: 0, Delta: StreamDelta{Content: prefix + word}},
			},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()

		// Simulate token generation delay
		time.Sleep(15 * time.Millisecond)
	}

	// Send finish chunk
	finishChunk := StreamChunk{
		ID:                streamID,
		Object:            "chat.completion.chunk",
		Created:           fullResp.Created,
		Model:             model,
		SystemFingerprint: fullResp.SystemFingerprint,
		Choices: []StreamChoice{
			{Index: 0, Delta: StreamDelta{}, FinishReason: strPtr("stop")},
		},
	}
	data, _ = json.Marshal(finishChunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()

	// Send [DONE] sentinel
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ─── Usage Emission ───────────────────────────────────────────

type usageEventPayload struct {
	RequestID    string  `json:"request_id"`
	DeploymentID string  `json:"deployment_id"`
	ReplicaID    string  `json:"replica_id"`
	HostID       string  `json:"host_id"`
	ModelName    string  `json:"model_name"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	GpuSeconds   float64 `json:"gpu_seconds"`
	Tier         string  `json:"tier"`
	Status       string  `json:"status"`
}

func (h *InferenceHandler) emitUsageEvent(requestID string, routeResp *routeResponseBody, req ChatCompletionRequest, orgID string) {
	// Parse usage from response
	var fullResp ChatCompletionResponse
	inputTokens, outputTokens := 0, 0
	if err := json.Unmarshal([]byte(routeResp.Body), &fullResp); err == nil {
		inputTokens = fullResp.Usage.PromptTokens
		outputTokens = fullResp.Usage.CompletionTokens
	} else {
		// Estimate tokens
		for _, msg := range req.Messages {
			inputTokens += len(msg.Content) / 4
		}
		outputTokens = len(routeResp.Body) / 4
	}

	// Resolve deployment ID from model name
	deploymentID := ""
	depQuery := `
		SELECT d.id FROM deployments d
		JOIN models m ON m.id = d.model_id
		WHERE m.name = $1 AND d.org_id = $2 AND d.deleted_at IS NULL
		ORDER BY d.created_at DESC LIMIT 1;
	`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.db.Pool.QueryRow(ctx, depQuery, req.Model, orgID).Scan(&deploymentID)
	if deploymentID == "" {
		deploymentID = "unknown"
	}

	payload := usageEventPayload{
		RequestID:    requestID,
		DeploymentID: deploymentID,
		ReplicaID:    routeResp.ReplicaID,
		HostID:       routeResp.HostID,
		ModelName:    req.Model,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		GpuSeconds:   0.5, // Estimated for dev mode
		Tier:         "t2",
		Status:       "success",
	}

	body, _ := json.Marshal(payload)
	url := h.billingURL + "/v1/usage"

	if routeResp == nil || routeResp.ReplicaID == "" || routeResp.HostID == "" {
		return
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if h.svcAuth != nil {
		h.svcAuth.SignRequest(httpReq)
	}

	resp, err := h.httpClient.Do(httpReq)
	if err != nil {
		fmt.Printf("gateway: WARNING failed to emit usage event: %v\n", err)
		return
	}
	resp.Body.Close()
}

// ─── Helpers ──────────────────────────────────────────────────

func writeGatewayError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    errType,
			"code":    status,
		},
	})
}

func strPtr(s string) *string {
	return &s
}

