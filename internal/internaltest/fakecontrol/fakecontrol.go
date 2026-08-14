// Package fakecontrol implements an in-memory fake of the Substrate ateapi
// Control service for tests. It mimics the control plane's lifecycle
// semantics: actors are created suspended, resume/suspend/pause flip
// status, and only suspended actors can be deleted.
package fakecontrol

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Server is a fake ateapi Control server.
type Server struct {
	ateapipb.UnimplementedControlServer

	mu        sync.Mutex
	actors    map[string]*ateapipb.Actor
	atespaces map[string]*ateapipb.Atespace
	snapshots map[string]*ateapipb.ActorSnapshot
	tags      map[string]*ateapipb.ActorSnapshotTag
	snapshotN int
}

// New returns an empty fake control server.
func New() *Server {
	return &Server{
		actors:    make(map[string]*ateapipb.Actor),
		atespaces: make(map[string]*ateapipb.Atespace),
		snapshots: make(map[string]*ateapipb.ActorSnapshot),
		tags:      make(map[string]*ateapipb.ActorSnapshotTag),
	}
}

// SetStatus forces the status of the actor with the given name, so tests can
// stage states the fake's own lifecycle transitions do not produce.
func (s *Server) SetStatus(name string, st ateapipb.Actor_Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.find(name); a != nil {
		a.Status = st
	}
}

// SnapshotOf returns the name of the snapshot the actor with the given name was
// created from, or "" when it was created empty.
func (s *Server) SnapshotOf(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.find(name).GetLatestSnapshot().GetName()
}

// Serve starts the fake on a random localhost port and returns its
// address and a shutdown function. Like the real ateapi, it serves TLS
// with a certificate no client can verify (clients set SkipVerify).
func (s *Server) Serve() (addr string, stop func(), err error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	cert, err := selfSignedCert()
	if err != nil {
		lis.Close()
		return "", nil, err
	}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
	})))
	ateapipb.RegisterControlServer(grpcServer, s)
	go grpcServer.Serve(lis)
	return lis.Addr().String(), grpcServer.Stop, nil
}

func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fakecontrol"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func key(atespace, name string) string { return atespace + "/" + name }

// Status returns the current status of the actor with the given name, or
// STATUS_UNSPECIFIED if it does not exist.
func (s *Server) Status(name string) ateapipb.Actor_Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.find(name).GetStatus()
}

// find returns the actor with the given name, whichever atespace holds it, or
// nil when no actor has that name. The helpers above address actors by name
// alone so tests do not have to know which atespace a client placed them in.
func (s *Server) find(name string) *ateapipb.Actor {
	for _, a := range s.actors {
		if a.GetMetadata().GetName() == name {
			return a
		}
	}
	return nil
}

func (s *Server) get(ref *ateapipb.ObjectRef) (*ateapipb.Actor, error) {
	a, ok := s.actors[key(ref.GetAtespace(), ref.GetName())]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "actor %q not found", ref.GetName())
	}
	return a, nil
}

func clone(a *ateapipb.Actor) *ateapipb.Actor {
	return proto.Clone(a).(*ateapipb.Actor)
}

func (s *Server) GetActor(ctx context.Context, req *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.get(req.GetActor())
	if err != nil {
		return nil, err
	}
	return clone(a), nil
}

func (s *Server) CreateActor(ctx context.Context, req *ateapipb.CreateActorRequest) (*ateapipb.Actor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	actor := req.GetActor()
	name := actor.GetMetadata().GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "metadata.name is required")
	}
	k := key(actor.GetMetadata().GetAtespace(), name)
	if _, ok := s.actors[k]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "actor %q already exists", name)
	}
	a := clone(actor)
	if ref := req.GetSourceSnapshot(); ref != nil {
		// Mirror the control plane: a source snapshot must be referenced by a
		// tag, and an atespace-scoped tag cannot cross atespaces.
		tagRef, ok := ref.GetReference().(*ateapipb.ActorSnapshotRef_Tag)
		if !ok {
			return nil, status.Error(codes.FailedPrecondition, "source ActorSnapshot must be referenced by tag")
		}
		tag, ok := s.tags[key(tagRef.Tag.GetAtespace(), tagRef.Tag.GetName())]
		if !ok {
			return nil, status.Errorf(codes.NotFound, "snapshot tag %q not found", tagRef.Tag.GetName())
		}
		if tag.GetMetadata().GetAtespace() != actor.GetMetadata().GetAtespace() {
			return nil, status.Error(codes.FailedPrecondition, "ActorSnapshot tag is not published outside its Atespace")
		}
		a.LatestSnapshot = proto.Clone(tag.GetSnapshot()).(*ateapipb.ObjectRef)
	}
	a.Status = ateapipb.Actor_STATUS_RUNNING
	s.actors[k] = a
	return clone(a), nil
}

// TagActorSnapshot names a snapshot so it can be used as a CreateActor source.
func (s *Server) TagActorSnapshot(ctx context.Context, req *ateapipb.TagActorSnapshotRequest) (*ateapipb.ActorSnapshotTag, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := req.GetSnapshot().GetReference().(*ateapipb.ActorSnapshotRef_Snapshot)
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "snapshot reference is required")
	}
	if _, ok := s.snapshots[key(ref.Snapshot.GetAtespace(), ref.Snapshot.GetName())]; !ok {
		return nil, status.Errorf(codes.NotFound, "snapshot %q not found", ref.Snapshot.GetName())
	}
	meta := req.GetTag().GetMetadata()
	if meta.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "tag metadata.name is required")
	}
	k := key(meta.GetAtespace(), meta.GetName())
	if _, ok := s.tags[k]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "snapshot tag %q already exists", meta.GetName())
	}
	tag := proto.Clone(req.GetTag()).(*ateapipb.ActorSnapshotTag)
	tag.Snapshot = proto.Clone(ref.Snapshot).(*ateapipb.ObjectRef)
	s.tags[k] = tag
	return proto.Clone(tag).(*ateapipb.ActorSnapshotTag), nil
}

func (s *Server) ResumeActor(ctx context.Context, req *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.get(req.GetActor())
	if err != nil {
		return nil, err
	}
	a.Status = ateapipb.Actor_STATUS_RUNNING
	a.WorkerAssignment = &ateapipb.WorkerAssignment{
		WorkerNamespace: "ate-system",
		WorkerPod:       "worker-0",
		WorkerPodIp:     "10.0.0.1",
	}
	return &ateapipb.ResumeActorResponse{Actor: clone(a)}, nil
}

func (s *Server) SuspendActor(ctx context.Context, req *ateapipb.SuspendActorRequest) (*ateapipb.SuspendActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.get(req.GetActor())
	if err != nil {
		return nil, err
	}
	s.suspend(a)
	return &ateapipb.SuspendActorResponse{Actor: clone(a)}, nil
}

// Suspend suspends the actor with the given name as the real control plane does
// when an environment goes idle, and returns the name of the snapshot that
// checkpoint wrote. Tests use it to stage a forkable source.
func (s *Server) Suspend(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.find(name)
	if a == nil {
		return ""
	}
	s.suspend(a)
	return a.GetLatestSnapshot().GetName()
}

// suspend checkpoints a: it writes a durable snapshot and records it as the
// actor's latest. The caller holds s.mu.
func (s *Server) suspend(a *ateapipb.Actor) {
	a.Status = ateapipb.Actor_STATUS_SUSPENDED
	a.WorkerAssignment = nil

	s.snapshotN++
	ref := &ateapipb.ObjectRef{
		Atespace: a.GetMetadata().GetAtespace(),
		Name:     fmt.Sprintf("%s-snapshot-%d", a.GetMetadata().GetName(), s.snapshotN),
	}
	s.snapshots[key(ref.GetAtespace(), ref.GetName())] = &ateapipb.ActorSnapshot{
		Metadata:               &ateapipb.ResourceMetadata{Atespace: ref.GetAtespace(), Name: ref.GetName()},
		SourceActor:            &ateapipb.ObjectRef{Atespace: ref.GetAtespace(), Name: a.GetMetadata().GetName()},
		ActorTemplateNamespace: a.GetActorTemplateNamespace(),
		ActorTemplateName:      a.GetActorTemplateName(),
	}
	a.LatestSnapshot = ref
}

func (s *Server) PauseActor(ctx context.Context, req *ateapipb.PauseActorRequest) (*ateapipb.PauseActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.get(req.GetActor())
	if err != nil {
		return nil, err
	}
	a.Status = ateapipb.Actor_STATUS_PAUSED
	return &ateapipb.PauseActorResponse{Actor: clone(a)}, nil
}

func (s *Server) DeleteActor(ctx context.Context, req *ateapipb.DeleteActorRequest) (*ateapipb.Actor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := req.GetActor()
	a, err := s.get(ref)
	if err != nil {
		return nil, err
	}
	if a.GetStatus() != ateapipb.Actor_STATUS_SUSPENDED {
		return nil, status.Errorf(codes.FailedPrecondition, "actor %q is %s, only suspended actors can be deleted",
			ref.GetName(), a.GetStatus())
	}
	delete(s.actors, key(ref.GetAtespace(), ref.GetName()))
	return clone(a), nil
}

func (s *Server) ListActors(ctx context.Context, req *ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &ateapipb.ListActorsResponse{}
	for _, a := range s.actors {
		if req.GetAtespace() != "" && a.GetMetadata().GetAtespace() != req.GetAtespace() {
			continue
		}
		resp.Actors = append(resp.Actors, clone(a))
	}
	return resp, nil
}

func (s *Server) CreateAtespace(ctx context.Context, req *ateapipb.CreateAtespaceRequest) (*ateapipb.Atespace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	space := req.GetAtespace()
	name := space.GetMetadata().GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "metadata.name is required")
	}
	if _, ok := s.atespaces[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "atespace %q already exists", name)
	}
	a := proto.Clone(space).(*ateapipb.Atespace)
	s.atespaces[name] = a
	return proto.Clone(a).(*ateapipb.Atespace), nil
}

func (s *Server) GetAtespace(ctx context.Context, req *ateapipb.GetAtespaceRequest) (*ateapipb.Atespace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := req.GetAtespace().GetName()
	a, ok := s.atespaces[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "atespace %q not found", name)
	}
	return proto.Clone(a).(*ateapipb.Atespace), nil
}
