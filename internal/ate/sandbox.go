package ate

import (
	"context"
	"fmt"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// Status is the lifecycle state of a sandbox.
type Status string

const (
	StatusUnknown    Status = "unknown"
	StatusResuming   Status = "resuming"
	StatusRunning    Status = "running"
	StatusSuspending Status = "suspending"
	StatusSuspended  Status = "suspended"
	StatusPausing    Status = "pausing"
	StatusPaused     Status = "paused"
	StatusCrashed    Status = "crashed"
)

// Info is a point-in-time snapshot of a sandbox's control-plane state.
type Info struct {
	ID     string
	Status Status

	// Template and Namespace identify the ActorTemplate the sandbox
	// was created from.
	Template  string
	Namespace string

	// WorkerPod is the pod currently hosting the sandbox, when running.
	WorkerPod          string
	WorkerPodNamespace string
	WorkerPodIP        string
}

func statusFromProto(s ateapipb.Actor_Status) Status {
	switch s {
	case ateapipb.Actor_STATUS_RESUMING:
		return StatusResuming
	case ateapipb.Actor_STATUS_RUNNING:
		return StatusRunning
	case ateapipb.Actor_STATUS_SUSPENDING:
		return StatusSuspending
	case ateapipb.Actor_STATUS_SUSPENDED:
		return StatusSuspended
	case ateapipb.Actor_STATUS_PAUSING:
		return StatusPausing
	case ateapipb.Actor_STATUS_PAUSED:
		return StatusPaused
	case ateapipb.Actor_STATUS_CRASHED:
		return StatusCrashed
	default:
		return StatusUnknown
	}
}

func infoFromActor(a *ateapipb.Actor) Info {
	return Info{
		ID:                 a.GetMetadata().GetName(),
		Status:             statusFromProto(a.GetStatus()),
		Namespace:          a.GetActorTemplateNamespace(),
		Template:           a.GetActorTemplateName(),
		WorkerPod:          a.GetAteomPodName(),
		WorkerPodNamespace: a.GetAteomPodNamespace(),
		WorkerPodIP:        a.GetAteomPodIp(),
	}
}

// Sandbox is a handle to a single sandbox (a Substrate actor).
type Sandbox struct {
	id     string
	client *Client
}

// ID returns the sandbox's identifier.
func (s *Sandbox) ID() string { return s.id }

// Info fetches the sandbox's current control-plane state.
func (s *Sandbox) Info(ctx context.Context) (Info, error) {
	actor, err := s.client.control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: s.client.ref(s.id)})
	if err != nil {
		return Info{}, fmt.Errorf("sandbox: getting %q: %w", s.id, wrapGRPCError(err))
	}
	return infoFromActor(actor), nil
}

// Resume restores the sandbox from its latest snapshot onto an available
// worker. It is a no-op on the control plane if the sandbox is already
// running.
func (s *Sandbox) Resume(ctx context.Context) error {
	_, err := s.client.control.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: s.client.ref(s.id)})
	if err != nil {
		return fmt.Errorf("sandbox: resuming %q: %w", s.id, wrapGRPCError(err))
	}
	return nil
}

// Suspend snapshots the sandbox's full state (memory and filesystem) to
// external storage and frees its worker. The sandbox can later be resumed
// on any eligible worker.
func (s *Sandbox) Suspend(ctx context.Context) error {
	_, err := s.client.control.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: s.client.ref(s.id)})
	if err != nil {
		return fmt.Errorf("sandbox: suspending %q: %w", s.id, wrapGRPCError(err))
	}
	return nil
}

// Pause snapshots the sandbox but keeps the snapshot local to the node for
// faster resume. Unlike Suspend, the state does not survive node loss.
func (s *Sandbox) Pause(ctx context.Context) error {
	_, err := s.client.control.PauseActor(ctx, &ateapipb.PauseActorRequest{Actor: s.client.ref(s.id)})
	if err != nil {
		return fmt.Errorf("sandbox: pausing %q: %w", s.id, wrapGRPCError(err))
	}
	return nil
}

// Delete removes the sandbox permanently. Substrate only deletes suspended
// actors, so Delete suspends the sandbox first if it is not already
// suspended.
func (s *Sandbox) Delete(ctx context.Context) error {
	info, err := s.Info(ctx)
	if err != nil {
		return err
	}
	if info.Status != StatusSuspended {
		if err := s.Suspend(ctx); err != nil {
			return fmt.Errorf("sandbox: suspending %q before delete: %w", s.id, err)
		}
	}
	_, err = s.client.control.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: s.client.ref(s.id)})
	if err != nil {
		return fmt.Errorf("sandbox: deleting %q: %w", s.id, wrapGRPCError(err))
	}
	return nil
}

// WaitStatus polls until the sandbox reaches the given status or ctx is
// done.
func (s *Sandbox) WaitStatus(ctx context.Context, want Status) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := s.Info(ctx)
		if err != nil {
			return err
		}
		if info.Status == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sandbox: waiting for %q to become %s: %w (last status %s)",
				s.id, want, ctx.Err(), info.Status)
		case <-ticker.C:
		}
	}
}
