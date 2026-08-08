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

// Delete removes the actor permanently. Substrate only deletes suspended
// actors, so Delete suspends the actor first.
func (s *ActorClient) Delete(ctx context.Context) error {
	if _, err := s.client.control.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: s.client.ref(s.id)}); err != nil {
		return fmt.Errorf("actor: suspending %q: %w", s.id, wrapGRPCError(err))
	}
	_, err := s.client.control.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: s.client.ref(s.id)})
	if err != nil {
		return fmt.Errorf("actor: deleting %q: %w", s.id, wrapGRPCError(err))
	}
	return nil
}
