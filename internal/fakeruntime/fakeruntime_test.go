package fakeruntime

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, cfg Config, cached ...string) (*Runtime, string) {
	t.Helper()
	r := New(cfg, cached...)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	return r, srv.URL
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// The agent reads a download as newline-delimited JSON with "completed" and
// "total", and treats {"status":"success"} as the end (agent/src/runtime.rs pull).
func TestPullStreamsProgressThenSuccess(t *testing.T) {
	_, url := serve(t, Config{})
	resp := post(t, url+"/api/pull", `{"model":"gemma2:2b","stream":true}`)
	var lines []map[string]any
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var l map[string]any
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("line is not JSON: %q", sc.Text())
		}
		lines = append(lines, l)
	}
	if len(lines) < 3 || lines[len(lines)-1]["status"] != "success" {
		t.Fatalf("pull must end with success, got %v", lines)
	}
	progress := 0
	for _, l := range lines {
		if _, ok := l["completed"].(float64); ok {
			if l["total"].(float64) <= 0 {
				t.Fatalf("progress line without a total: %v", l)
			}
			progress++
		}
	}
	if progress == 0 {
		t.Fatal("no progress lines with completed/total")
	}

	var tags struct {
		Models []struct{ Name string } `json:"models"`
	}
	r, _ := http.Get(url + "/api/tags")
	decode(t, r, &tags)
	r.Body.Close()
	if len(tags.Models) != 1 || tags.Models[0].Name != "gemma2:2b" {
		t.Fatalf("a pulled model must be listed, got %+v", tags.Models)
	}
}

func TestFailedPullReportsTheErrorInTheStream(t *testing.T) {
	_, url := serve(t, Config{FailPull: []string{"missing:1b"}})
	resp := post(t, url+"/api/pull", `{"model":"missing:1b"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Ollama answers 200 and puts the failure in the stream; got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"error"`) || strings.Contains(string(body), `"success"`) {
		t.Fatalf("stream = %s, want an error line and no success", body)
	}
}

func TestLoadAndUnloadFollowKeepAlive(t *testing.T) {
	rt, url := serve(t, Config{FailLoad: []string{"big:70b"}}, "gemma2:2b", "big:70b")

	if resp := post(t, url+"/api/generate", `{"model":"nope","keep_alive":-1}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("loading an unknown model = %d, want 404", resp.StatusCode)
	}
	if resp := post(t, url+"/api/generate", `{"model":"big:70b","keep_alive":-1}`); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a model on the fail list loaded: %d", resp.StatusCode)
	}
	if resp := post(t, url+"/api/generate", `{"model":"gemma2:2b","keep_alive":-1}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("load = %d", resp.StatusCode)
	}
	if got := rt.Loaded(); len(got) != 1 || got[0] != "gemma2:2b" {
		t.Fatalf("loaded = %v, want gemma2:2b", got)
	}
	var ps struct {
		Models []struct{ Name string } `json:"models"`
	}
	r, _ := http.Get(url + "/api/ps")
	decode(t, r, &ps)
	r.Body.Close()
	if len(ps.Models) != 1 {
		t.Fatalf("/api/ps = %+v, want the loaded model", ps.Models)
	}
	if resp := post(t, url+"/api/generate", `{"model":"gemma2:2b","keep_alive":0}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("unload = %d", resp.StatusCode)
	}
	if got := rt.Loaded(); len(got) != 0 {
		t.Fatalf("still loaded after keep_alive 0: %v", got)
	}
}

func TestChatCompletionReportsUsageAndHonoursMaxTokens(t *testing.T) {
	_, url := serve(t, Config{}, "gemma2:2b")
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message      struct{ Role, Content string } `json:"message"`
			FinishReason string                         `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	decode(t, post(t, url+"/v1/chat/completions", `{"model":"gemma2:2b","messages":[{"role":"user","content":"hello there"}],"max_tokens":3}`), &out)
	if out.Model != "gemma2:2b" || len(out.Choices) != 1 || out.Choices[0].Message.Content == "" {
		t.Fatalf("unexpected completion: %+v", out)
	}
	if out.Usage.CompletionTokens != 3 || out.Choices[0].FinishReason != "length" || out.Usage.PromptTokens == 0 {
		t.Fatalf("usage %+v finish %q, want 3 completion tokens cut by length", out.Usage, out.Choices[0].FinishReason)
	}
	if n := len(strings.Fields(out.Choices[0].Message.Content)); n != 3 {
		t.Fatalf("content has %d words, want one per token (3)", n)
	}

	if resp := post(t, url+"/v1/chat/completions", `{"model":"other","messages":[]}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown model = %d, want 404", resp.StatusCode)
	}
}

// events returns the data payload of each server-sent event.
func events(t *testing.T, resp *http.Response) []string {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	var out []string
	for _, ev := range strings.Split(strings.TrimSpace(string(body)), "\n\n") {
		data, ok := strings.CutPrefix(ev, "data: ")
		if !ok {
			t.Fatalf("event without a data line: %q", ev)
		}
		out = append(out, data)
	}
	return out
}

// The agent counts one token per content delta and takes exact counts from a
// final chunk whose choices are empty (agent/src/inference.rs rewrite_event).
func TestStreamIsOneDeltaPerTokenWithUsageOnlyWhenAsked(t *testing.T) {
	_, url := serve(t, Config{Tokens: 5}, "gemma2:2b")
	req := `{"model":"gemma2:2b","messages":[{"role":"user","content":"hi"}],"stream":true`

	ev := events(t, post(t, url+"/v1/chat/completions", req+`,"stream_options":{"include_usage":true}}`))
	if ev[len(ev)-1] != "[DONE]" {
		t.Fatalf("stream must end with [DONE], got %q", ev[len(ev)-1])
	}
	deltas, usageChunks := 0, 0
	for _, e := range ev[:len(ev)-1] {
		var c struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(e), &c); err != nil {
			t.Fatalf("chunk is not JSON: %q", e)
		}
		if c.Model != "gemma2:2b" {
			t.Fatalf("every chunk carries the model, got %q", c.Model)
		}
		if c.Usage != nil {
			usageChunks++
			if len(c.Choices) != 0 || c.Usage.CompletionTokens != 5 {
				t.Fatalf("usage chunk must have empty choices and 5 tokens: %s", e)
			}
		}
		if len(c.Choices) > 0 && c.Choices[0].Delta.Content != "" {
			deltas++
		}
	}
	if deltas != 5 || usageChunks != 1 {
		t.Fatalf("%d content deltas and %d usage chunks, want 5 and 1", deltas, usageChunks)
	}

	for _, e := range events(t, post(t, url+"/v1/chat/completions", req+`}`)) {
		if strings.Contains(e, `"usage"`) {
			t.Fatalf("usage sent although the request did not ask for it: %s", e)
		}
	}
}

func TestEmbeddingsAndModelList(t *testing.T) {
	_, url := serve(t, Config{}, "embed:1")
	var emb struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	decode(t, post(t, url+"/v1/embeddings", `{"model":"embed:1","input":["one","two"]}`), &emb)
	if len(emb.Data) != 2 || len(emb.Data[1].Embedding) == 0 || emb.Data[1].Index != 1 || emb.Usage.PromptTokens == 0 {
		t.Fatalf("unexpected embeddings: %+v", emb)
	}
	decode(t, post(t, url+"/v1/embeddings", `{"model":"embed:1","input":"just one"}`), &emb)
	if len(emb.Data) != 1 {
		t.Fatalf("a single string input must give one vector, got %d", len(emb.Data))
	}

	var list struct {
		Data []struct{ ID string } `json:"data"`
	}
	r, _ := http.Get(url + "/v1/models")
	decode(t, r, &list)
	r.Body.Close()
	if len(list.Data) != 1 || list.Data[0].ID != "embed:1" {
		t.Fatalf("model list = %+v", list.Data)
	}
}

func TestFaultsCanBeSetOverHTTP(t *testing.T) {
	_, url := serve(t, Config{}, "gemma2:2b")
	chat := `{"model":"gemma2:2b","messages":[{"role":"user","content":"hi"}]}`
	if resp := post(t, url+"/v1/chat/completions", chat); resp.StatusCode != http.StatusOK {
		t.Fatalf("healthy runtime answered %d", resp.StatusCode)
	}
	if resp := post(t, url+"/_fake/config", `{"status":503}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("setting config = %d", resp.StatusCode)
	}
	if resp := post(t, url+"/v1/chat/completions", chat); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("forced status ignored: %d", resp.StatusCode)
	}
}
