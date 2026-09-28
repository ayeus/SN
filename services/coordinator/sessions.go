package main

import (
	"errors"
	"sync"

	agentv1 "github.com/ayeus/ayeusann/gen/go/agent/v1"
	"github.com/ayeus/ayeusann/internal/platform"
)

// errSessionClosed is delivered to in-flight inference requests when their
// host disconnects, so the gateway can retry on another replica.
var errSessionClosed = errors.New("host disconnected")

// session is one connected agent. gRPC forbids concurrent Send on a stream,
// and the dispatch loop, stop loop and inference tunnel all send, so every
// send goes through sendMu.
type session struct {
	hostID string
	stream agentv1.AgentService_SessionServer

	sendMu sync.Mutex

	mu       sync.Mutex
	inflight map[string]chan *agentv1.InferenceChunk
	closed   bool
}

func newSession(hostID string, stream agentv1.AgentService_SessionServer) *session {
	return &session{hostID: hostID, stream: stream, inflight: map[string]chan *agentv1.InferenceChunk{}}
}

func (s *session) send(msg *agentv1.CoordinatorMessage) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(msg)
}

// open registers an in-flight inference request and returns its chunk channel.
func (s *session) open(requestID string) (chan *agentv1.InferenceChunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errSessionClosed
	}
	ch := make(chan *agentv1.InferenceChunk, 256)
	s.inflight[requestID] = ch
	return ch, nil
}

// release forgets an in-flight request.
func (s *session) release(requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, requestID)
}

// deliver routes a chunk from the agent to the waiting HTTP handler. It blocks
// when the handler is slow, which applies back-pressure to the agent stream.
func (s *session) deliver(c *agentv1.InferenceChunk, done <-chan struct{}) {
	s.mu.Lock()
	ch, ok := s.inflight[c.GetRequestId()]
	s.mu.Unlock()
	if !ok {
		return // request already finished or cancelled
	}
	select {
	case ch <- c:
	case <-done:
	}
}

// close fails every in-flight request with a synthetic error chunk.
func (s *session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for id, ch := range s.inflight {
		select {
		case ch <- &agentv1.InferenceChunk{RequestId: id, Done: true, StatusCode: 502, Error: errSessionClosed.Error()}:
		default:
		}
		delete(s.inflight, id)
	}
}

// registry maps host id to its live session.
type registry struct {
	mu     sync.RWMutex
	byHost map[string]*session
}

func newRegistry() *registry { return &registry{byHost: map[string]*session{}} }

func (r *registry) get(hostID string) *session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byHost[hostID]
}

// put installs a session, returning any previous session for the same host so
// the caller can close it (an agent that reconnects replaces its old stream).
func (r *registry) put(s *session) *session {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.byHost[s.hostID]
	r.byHost[s.hostID] = s
	platform.HostsConnected.Set(float64(len(r.byHost)))
	return old
}

// remove deletes a session only if it is still the current one for its host.
func (r *registry) remove(s *session) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byHost[s.hostID]
	if !ok || cur != s {
		return false
	}
	delete(r.byHost, s.hostID)
	platform.HostsConnected.Set(float64(len(r.byHost)))
	return true
}

func (r *registry) connected() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byHost))
	for id := range r.byHost {
		out = append(out, id)
	}
	return out
}
