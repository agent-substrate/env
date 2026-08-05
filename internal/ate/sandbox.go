package ate

import (
	"context"
	"fmt"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// SandboxClient is a handle to a single sandbox (a Substrate actor).
type SandboxClient struct {
	id     string
	client *Client
}

// ID returns the sandbox's identifier.
func (s *SandboxClient) ID() string { return s.id }

// Resume restores the sandbox from its latest snapshot onto an available
// worker. It is a no-op on the control plane if the sandbox is already
// running.
func (s *SandboxClient) Resume(ctx context.Context) error {
	_, err := s.client.control.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: s.client.ref(s.id)})
	if err != nil {
		return fmt.Errorf("sandbox: resuming %q: %w", s.id, wrapGRPCError(err))
	}
	return nil
}

// Suspend snapshots the sandbox's full state (memory and filesystem) to
// external storage and frees its worker. The sandbox can later be resumed
// on any eligible worker.
func (s *SandboxClient) Suspend(ctx context.Context) error {
	_, err := s.client.control.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: s.client.ref(s.id)})
	if err != nil {
		return fmt.Errorf("sandbox: suspending %q: %w", s.id, wrapGRPCError(err))
	}
	return nil
}

// Delete removes the sandbox permanently. Substrate only deletes suspended
// actors, so Delete suspends the sandbox first.
func (s *SandboxClient) Delete(ctx context.Context) error {
	_, err := s.client.control.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: s.client.ref(s.id)})
	if err != nil {
		return fmt.Errorf("sandbox: deleting %q: %w", s.id, wrapGRPCError(err))
	}
	return nil
}
