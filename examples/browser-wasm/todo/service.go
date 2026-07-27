package todo

import (
	"context"
	"strings"

	"github.com/lesomnus/grpc-dgram/examples/browser-wasm/todopb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Service is an ordinary gRPC service implementation: generated stubs, request
// validation, real statuses, one server-streaming method. Nothing in it knows
// what carries its calls — a socket, or the other end of a MessageChannel in
// the same tab.
type Service struct {
	todopb.UnimplementedTodoServiceServer

	store    Store
	servedBy string
}

// NewService returns the service backed by store. servedBy is what List
// reports — "the js/wasm build" for the one that runs in the browser — so the
// status line names which build answered instead of assuming it: the page
// renders what the server told it. Where that build runs is the host's choice
// and not this server's to claim; the page's own half of the line says so.
func NewService(store Store, servedBy string) *Service {
	return &Service{store: store, servedBy: servedBy}
}

func (s *Service) List(context.Context, *todopb.ListRequest) (*todopb.ListResponse, error) {
	return &todopb.ListResponse{Tasks: s.store.List(), ServedBy: s.servedBy}, nil
}

func (s *Service) Add(_ context.Context, req *todopb.AddRequest) (*todopb.Task, error) {
	title := strings.TrimSpace(req.GetTitle())
	if title == "" {
		// A plain gRPC status: it travels as the call's terminal frame and
		// surfaces in the page as a StatusError with code 3.
		return nil, status.Error(codes.InvalidArgument, "title must not be empty")
	}
	return s.store.Add(title), nil
}

func (s *Service) Toggle(_ context.Context, req *todopb.ToggleRequest) (*todopb.Task, error) {
	t, ok := s.store.Toggle(req.GetId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no task %d", req.GetId())
	}
	return t, nil
}

func (s *Service) Remove(_ context.Context, req *todopb.RemoveRequest) (*todopb.RemoveResponse, error) {
	t, ok := s.store.Remove(req.GetId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no task %d", req.GetId())
	}
	return &todopb.RemoveResponse{Id: t.GetId()}, nil
}

// Watch streams every mutation until the client goes away. It is the method
// that makes the page a subscriber rather than a poller, and the one that is
// most tedious to fake by hand — which is the argument for running the real
// server in the page.
func (s *Service) Watch(_ *todopb.WatchRequest, stream grpc.ServerStreamingServer[todopb.Event]) error {
	ctx := stream.Context()
	events, unsubscribe := s.store.Watch()
	defer unsubscribe()

	for {
		select {
		case <-ctx.Done():
			// The client left, its transport died, or the server is stopping.
			// In reliable mode nothing else cancels a parked handler: this is
			// the adapter's §4.5 teardown reaching the handler, and returning
			// here is what releases the subscription.
			return status.FromContextError(ctx.Err()).Err()
		case ev, ok := <-events:
			if !ok {
				return status.Error(codes.ResourceExhausted, "watcher fell behind; re-subscribe and List again")
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}
