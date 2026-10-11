package main

import (
	"errors"
	"sync"
	"time"

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
	// What the agent said about itself when it registered.
	agentVersion string
	capabilities []string

	sendMu sync.Mutex

	// When the host's model runtime was first reported unreachable, and
	// whether its replicas have been given up on since. Touched only by the
	// goroutine that reads this session's stream.
	runtimeDownSince time.Time
	runtimeGivenUp   bool

	mu       sync.Mutex
	inflight map[string]*answer
	closed   bool
	// done is closed with the session. The stream's handler watches it, so
	// closing a session really does end the connection: the agent sees the
	// stream finish and reconnects, instead of holding a line nobody reads.
	done chan struct{}
}

func newSession(hostID string, stream agentv1.AgentService_SessionServer) *session {
	return &session{hostID: hostID, stream: stream, inflight: map[string]*answer{}, done: make(chan struct{})}
}

func (s *session) send(msg *agentv1.CoordinatorMessage) error {
	select {
	case <-s.done:
		return errSessionClosed
	default:
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(msg)
}

// Protocol features an agent may list when it registers.
const (
	// capKeepalive: the agent says "still working" while a runtime has
	// produced nothing, so silence means the host is gone, not thinking.
	capKeepalive = "inference-keepalive"
	// capLargeMessages: the agent accepts messages up to
	// platform.MaxAgentMessage. Older agents drop the connection at 4 MB.
	capLargeMessages = "large-messages"
)

// has reports whether the agent listed a capability when it registered.
func (s *session) has(capability string) bool {
	for _, c := range s.capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// maxBufferedAnswer bounds what one request may have waiting for a reader
// that is not keeping up. It is above the largest answer an agent sends in
// one piece of work (32 MB, non-streamed), so only a stalled reader meets it.
const maxBufferedAnswer = 48 << 20

// errSlowReader ends a request whose reader fell too far behind.
const errSlowReader = "the answer was not being read and outgrew its buffer"

// answer holds the chunks of one request until its HTTP handler takes them.
//
// It never makes the sender wait. Everything an agent sends, heartbeats
// included, arrives on one stream read by one goroutine; if a chunk could
// wait here for a slow reader, that host's heartbeats would wait behind it
// and the host would be declared offline for somebody else's slow download.
type answer struct {
	mu     sync.Mutex
	chunks []*agentv1.InferenceChunk
	bytes  int
	ended  bool // the final chunk is queued; anything later is dropped
	// wake holds at most one token: "there is something to take".
	wake chan struct{}
}

func newAnswer() *answer { return &answer{wake: make(chan struct{}, 1)} }

// push queues a chunk. It reports false, once, when the queue has outgrown
// its limit: what was waiting is dropped and the request ends with an error.
func (a *answer) push(c *agentv1.InferenceChunk) bool {
	a.mu.Lock()
	ok := true
	switch {
	case a.ended:
	case a.bytes+len(c.GetData()) > maxBufferedAnswer:
		a.chunks = []*agentv1.InferenceChunk{{RequestId: c.GetRequestId(), Done: true, StatusCode: 502, Error: errSlowReader}}
		a.bytes, a.ended, ok = 0, true, false
	default:
		a.chunks = append(a.chunks, c)
		a.bytes += len(c.GetData())
		a.ended = c.GetDone()
	}
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
	return ok
}

// pop takes the oldest waiting chunk, or nil when there is none.
func (a *answer) pop() *agentv1.InferenceChunk {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.chunks) == 0 {
		return nil
	}
	c := a.chunks[0]
	a.chunks[0] = nil
	a.chunks = a.chunks[1:]
	a.bytes -= len(c.GetData())
	return c
}

// open registers an in-flight inference request and returns its answer queue.
func (s *session) open(requestID string) (*answer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errSessionClosed
	}
	a := newAnswer()
	s.inflight[requestID] = a
	return a, nil
}

// release forgets an in-flight request.
func (s *session) release(requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, requestID)
}

// deliver hands a chunk from the agent to the request waiting for it. It
// returns at once whatever the reader is doing.
func (s *session) deliver(c *agentv1.InferenceChunk) {
	s.mu.Lock()
	a, ok := s.inflight[c.GetRequestId()]
	s.mu.Unlock()
	if !ok {
		return // request already finished or cancelled
	}
	if !a.push(c) {
		// Nobody is reading this answer; tell the host to stop producing it.
		// Sending can wait on the agent, so it is not done on this goroutine.
		id := c.GetRequestId()
		go func() {
			_ = s.send(&agentv1.CoordinatorMessage{Payload: &agentv1.CoordinatorMessage_InferenceCancel{
				InferenceCancel: &agentv1.InferenceCancel{RequestId: id},
			}})
		}()
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
	close(s.done)
	for id, a := range s.inflight {
		a.push(&agentv1.InferenceChunk{RequestId: id, Done: true, StatusCode: 502, Error: errSessionClosed.Error()})
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

// drop closes a host's session and takes it out of the registry at once, so
// nothing is sent to a host that has been given up on.
func (r *registry) drop(hostID string) {
	r.mu.Lock()
	s := r.byHost[hostID]
	delete(r.byHost, hostID)
	platform.HostsConnected.Set(float64(len(r.byHost)))
	r.mu.Unlock()
	if s != nil {
		s.close()
	}
}

// dropIf closes a session and takes it out of the registry, but only while it
// is still the host's current one. A caller holding a session it fetched a
// moment ago must not throw out the newer session of a host that has
// reconnected since.
func (r *registry) dropIf(s *session) {
	r.mu.Lock()
	if r.byHost[s.hostID] == s {
		delete(r.byHost, s.hostID)
		platform.HostsConnected.Set(float64(len(r.byHost)))
	}
	r.mu.Unlock()
	s.close()
}

// closeAll ends every session, for shutdown.
func (r *registry) closeAll() {
	r.mu.Lock()
	all := make([]*session, 0, len(r.byHost))
	for _, s := range r.byHost {
		all = append(all, s)
	}
	r.byHost = map[string]*session{}
	platform.HostsConnected.Set(0)
	r.mu.Unlock()
	for _, s := range all {
		s.close()
	}
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

// all returns the current sessions.
func (r *registry) all() []*session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*session, 0, len(r.byHost))
	for _, s := range r.byHost {
		out = append(out, s)
	}
	return out
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
