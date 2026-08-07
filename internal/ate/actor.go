package ate

import (
	"context"
	"fmt"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// ActorClient is a handle to a single actor.
type ActorClient struct {
	id     string
	client *Client
}

// ID returns the actor's identifier.
func (s *ActorClient) ID() string { return s.id }

// Resume restores the actor from its latest snapshot onto an available
// worker. It is a no-op on the control plane if the environment is already
// running.
func (s *ActorClient) Resume(ctx context.Context) error {
	_, err := s.client.control.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: s.client.ref(s.id)})
	if err != nil {
		return fmt.Errorf("actor: resuming %q: %w", s.id, wrapGRPCError(err))
	}
	return nil
}

// Suspend snapshots the actor's full state (memory and filesystem) to
// external storage and frees its worker. The actor can later be resumed
// on any eligible worker.
func (s *ActorClient) Suspend(ctx context.Context) error {
	_, err := s.client.control.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: s.client.ref(s.id)})
	if err != nil {
		return fmt.Errorf("actor: suspending %q: %w", s.id, wrapGRPCError(err))
	}
	return nil
}

// Delete removes the actor permanently. Substrate only deletes suspended
// actors, so Delete suspends the actor first.
func (s *ActorClient) Delete(ctx context.Context) error {
	if err := s.Suspend(ctx); err != nil {
		return err
	}
	_, err := s.client.control.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: s.client.ref(s.id)})
	if err != nil {
		return fmt.Errorf("actor: deleting %q: %w", s.id, wrapGRPCError(err))
	}
	return nil
}
