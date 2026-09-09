package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func createMockRouteResponse(model, requestID, content string) *routeResponseBody {
	resp := ChatCompletionResponse{
		ID:                "chatcmpl-" + requestID[:12],
		Object:            "chat.completion",
		Created:           time.Now().Unix(),
		Model:             model,
		SystemFingerprint: "fp_test",
		Choices: []Choice{
			{
				Index: 0,
				Message: Message{
					Role:    "assistant",
					Content: content,
				},
				FinishReason: "stop",
			},
		},
		Usage: Usage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
	}
	respJSON, _ := json.Marshal(resp)
	return &routeResponseBody{
		ReplicaID:  "test-replica-1",
		HostID:     "test-host-1",
		StatusCode: http.StatusOK,
		Body:       string(respJSON),
	}
}

func TestHandleChatCompletions_MissingAPIKey(t *testing.T) {
	handler := &InferenceHandler{
		httpClient: http.DefaultClient,
	}

	body := `{"model":"llama-3.1-8b-instruct","messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()

	handler.HandleChatCompletions(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	var errResp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatal("expected error object in response")
	}
	if errObj["type"] != "authentication_error" {
		t.Fatalf("expected authentication_error type, got: %v", errObj["type"])
	}
}

func TestHandleChatCompletions_InvalidAPIKeyFormat(t *testing.T) {
	handler := &InferenceHandler{
		httpClient: http.DefaultClient,
	}

	body := `{"model":"llama-3.1-8b-instruct","messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-API-Key", "invalid_key_format")
	w := httptest.NewRecorder()

	handler.HandleChatCompletions(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleChatCompletions_CORS(t *testing.T) {
	handler := &InferenceHandler{
		httpClient: http.DefaultClient,
	}

	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	handler.HandleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for OPTIONS, got %d", w.Code)
	}

	cors := w.Header().Get("Access-Control-Allow-Origin")
	if cors != "*" {
		t.Fatalf("expected CORS header '*', got '%s'", cors)
	}

	methods := w.Header().Get("Access-Control-Allow-Methods")
	if !strings.Contains(methods, "POST") {
		t.Fatalf("expected POST in allowed methods, got '%s'", methods)
	}
}

func TestWriteNonStreamResponse_ParsedJSON(t *testing.T) {
	handler := &InferenceHandler{
		httpClient: http.DefaultClient,
	}

	routeResp := createMockRouteResponse("llama-3.1-8b-instruct", "test-request-id-1234567890", "Hello from worker!")
	w := httptest.NewRecorder()
	handler.writeNonStreamResponse(w, routeResp, "llama-3.1-8b-instruct", "test-request-id-1234567890")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var chatResp ChatCompletionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &chatResp); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}

	if chatResp.Model != "llama-3.1-8b-instruct" {
		t.Fatalf("expected model llama-3.1-8b-instruct, got %s", chatResp.Model)
	}
	if len(chatResp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(chatResp.Choices))
	}
	if chatResp.Choices[0].Message.Content != "Hello from worker!" {
		t.Fatalf("unexpected content: %s", chatResp.Choices[0].Message.Content)
	}
}

func TestSSEStream_FormatValidation(t *testing.T) {
	handler := &InferenceHandler{
		httpClient: http.DefaultClient,
	}

	req := ChatCompletionRequest{
		Model:  "llama-3.1-8b-instruct",
		Stream: true,
		Messages: []Message{
			{Role: "user", Content: "Hi"},
		},
	}

	routeResp := createMockRouteResponse(req.Model, "stream-test-id-1234567890", "Hi there")

	w := httptest.NewRecorder()
	handler.writeSSEStream(w, routeResp, req.Model, "stream-test-id-1234567890")

	if w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("expected text/event-stream content type, got %s", w.Header().Get("Content-Type"))
	}

	body := w.Body.String()

	// Verify SSE format: lines start with "data: "
	if !strings.Contains(body, "data: ") {
		t.Fatal("expected SSE data lines")
	}

	// Verify [DONE] sentinel
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatal("expected [DONE] sentinel in SSE stream")
	}

	// Verify role chunk is present
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Fatal("expected role chunk with assistant role")
	}

	// Verify finish_reason chunk
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatal("expected finish_reason stop in stream")
	}
}

func TestWriteGatewayError_Format(t *testing.T) {
	w := httptest.NewRecorder()
	writeGatewayError(w, http.StatusBadRequest, "invalid_request_error", "Test error message")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}

	errObj, ok := resp["error"].(map[string]interface{})
	if !ok {
		t.Fatal("expected error object")
	}

	if errObj["message"] != "Test error message" {
		t.Fatalf("expected error message, got: %v", errObj["message"])
	}
	if errObj["type"] != "invalid_request_error" {
		t.Fatalf("expected error type, got: %v", errObj["type"])
	}
	if errObj["code"].(float64) != 400 {
		t.Fatalf("expected error code 400, got: %v", errObj["code"])
	}
}

func TestWriteNonStreamResponse(t *testing.T) {
	handler := &InferenceHandler{
		httpClient: http.DefaultClient,
	}

	routeResp := &routeResponseBody{
		ReplicaID:  "r1",
		HostID:     "h1",
		StatusCode: 200,
		Body:       `{"id":"chatcmpl-test","choices":[{"message":{"content":"Hello!"}}]}`,
	}

	w := httptest.NewRecorder()
	handler.writeNonStreamResponse(w, routeResp, "test-model", "req-123")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if w.Header().Get("X-Request-ID") != "req-123" {
		t.Fatal("expected X-Request-ID header")
	}

	if w.Body.String() != routeResp.Body {
		t.Fatalf("expected body to match, got: %s", w.Body.String())
	}
}

func TestStrPtr(t *testing.T) {
	s := "test"
	p := strPtr(s)
	if p == nil {
		t.Fatal("expected non-nil pointer")
	}
	if *p != "test" {
		t.Fatalf("expected 'test', got '%s'", *p)
	}
}
