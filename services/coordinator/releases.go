package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io/fs"
	"log/slog"
	"sync"
	"time"

	agentv1 "github.com/ayeus/ayeusann/gen/go/agent/v1"
	"github.com/ayeus/ayeusann/internal/release"
)

// capSelfUpdate is the capability of an agent that can replace its own binary
// from a signed release.
const capSelfUpdate = "self-update"

// releases watches the published agent release and knows what to tell agents
// about it. The coordinator holds only the release key's public half: it can
// check a release and pass it on, never produce one.
type releases struct {
	dir         string
	pub         ed25519.PublicKey
	downloadURL string
	log         *slog.Logger

	mu       sync.RWMutex
	manifest *release.Manifest
	body     []byte
	sig      string
	lastErr  string
}

// newReleases returns nil when the installation publishes no updates (no
// release key configured).
func newReleases(dir string, pub ed25519.PublicKey, downloadURL string, log *slog.Logger) *releases {
	if len(pub) == 0 || dir == "" {
		return nil
	}
	r := &releases{dir: dir, pub: pub, downloadURL: downloadURL, log: log}
	r.reload()
	return r
}

// reload reads the release in the directory and reports whether a different
// version is now current. A release that does not verify is ignored, and the
// last good one stays in force: a platform that has been tampered with must
// not be able to roll hosts onto something unsigned.
func (r *releases) reload() bool {
	m, body, sig, err := release.Load(r.dir, r.pub)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) && err.Error() != r.lastErr {
			r.lastErr = err.Error()
			r.log.Error("the published agent release was ignored", "dir", r.dir, "err", err)
		}
		return false
	}
	r.lastErr = ""
	// Any change to what was signed counts, not only a new version number:
	// the same version published again with a build that was missing before
	// has to reach the machines that were waiting for it.
	changed := r.manifest == nil || string(r.body) != string(body)
	r.manifest, r.body, r.sig = m, body, sig
	if changed {
		r.log.Info("agent release loaded", "version", m.Version, "min_version", m.MinVersion, "artifacts", len(m.Artifacts))
	}
	return changed
}

// publicKey is what agents pin.
func (r *releases) publicKey() []byte {
	if r == nil {
		return nil
	}
	return r.pub
}

// outdated reports whether an agent of this version is below the minimum and
// must not be given work.
func (r *releases) outdated(agentVersion string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.manifest != nil && r.belowMinimum(agentVersion)
}

// belowMinimum reports whether a version is under the release's minimum. A
// minimum of 0.0.0 (the default when the publisher names none) is no minimum:
// nobody is out of date, including agents whose version cannot be read.
func (r *releases) belowMinimum(agentVersion string) bool {
	min := r.manifest.MinVersion
	if min == "" || release.Compare(min, "0.0.0") <= 0 {
		return false
	}
	return release.Compare(agentVersion, min) < 0
}

// offer returns the message that tells an agent of this version about a newer
// release, or nil when it is current, cannot update itself, or nothing is
// published.
func (r *releases) offer(agentVersion string, capabilities []string) *agentv1.CoordinatorMessage {
	if r == nil {
		return nil
	}
	can := false
	for _, c := range capabilities {
		if c == capSelfUpdate {
			can = true
		}
	}
	if !can {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.manifest == nil || release.Compare(agentVersion, r.manifest.Version) >= 0 {
		return nil
	}
	return &agentv1.CoordinatorMessage{Payload: &agentv1.CoordinatorMessage_Update{Update: &agentv1.UpdateAvailable{
		Version:     r.manifest.Version,
		DownloadUrl: r.downloadURL,
		Manifest:    r.body,
		Signature:   []byte(r.sig),
		Mandatory:   r.belowMinimum(agentVersion),
	}}}
}

// watch reloads the release every interval and, when a new one appears, tells
// every connected agent that is behind.
func (s *AgentServer) watchReleases(ctx context.Context, interval time.Duration) {
	if s.releases == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s.releases.reload() {
			s.announceRelease(ctx)
		}
	}
}

// announceRelease offers the current release to connected agents and brings
// each host's "out of date" mark in line with the new minimum.
func (s *AgentServer) announceRelease(ctx context.Context) {
	for _, sess := range s.sessions.all() {
		// A host that has reconnected since the list was taken is looked
		// after by its own registration, with its current version.
		if s.sessions.get(sess.hostID) != sess {
			continue
		}
		if _, err := s.db.Pool.Exec(ctx, `UPDATE hosts SET agent_outdated = $2 WHERE id = $1 AND agent_outdated <> $2;`,
			sess.hostID, s.releases.outdated(sess.agentVersion)); err != nil {
			s.log.Error("could not update a host's version status", "host_id", sess.hostID, "err", err)
		}
		if msg := s.releases.offer(sess.agentVersion, sess.capabilities); msg != nil {
			if err := sess.send(msg); err != nil {
				s.sessions.dropIf(sess)
			}
		}
	}
}
