package drpc

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var _ grpc.ServiceRegistrar = &Server{}

type Server struct {
	mu sync.Mutex
	tx FrameHandler
	ss map[uint32]*serverStream

	// methods is a mapping from method name to index.
	// Key must be a full method name (e.g. "/hday.HolderService/Add").
	methods  []*serviceDesc
	services map[string]*serviceDesc

	wg sync.WaitGroup
}

func NewServer(tx FrameHandler) *Server {
	return &Server{
		tx: tx,
		ss: map[uint32]*serverStream{},

		methods:  []*serviceDesc{},
		services: map[string]*serviceDesc{},
	}
}

func (s *Server) RegisterService(desc *grpc.ServiceDesc, impl any) {
	for i, method := range desc.Methods {
		fullname := fmt.Sprintf("/%s/%s", desc.ServiceName, method.MethodName)
		d, ok := s.services[fullname]
		if !ok {
			d = &serviceDesc{
				index:    uint32(len(s.methods)),
				fullname: fullname,
			}
			s.services[fullname] = d
			s.methods = append(s.methods, d)
		}

		d.service = desc
		d.method = &desc.Methods[i]
		d.impl = impl
	}
	for i, stream := range desc.Streams {
		fullname := fmt.Sprintf("/%s/%s", desc.ServiceName, stream.StreamName)
		d, ok := s.services[fullname]
		if !ok {
			d = &serviceDesc{
				index:    uint32(len(s.methods)),
				fullname: fullname,
			}
			s.services[fullname] = d
			s.methods = append(s.methods, d)
		}

		d.service = desc
		d.stream = &desc.Streams[i]
		d.impl = impl
	}
}

func (s *Server) Handle(ctx context.Context, req *Frame) error {
	sid := req.GetSid()
	res := Frame_builder{
		Sid:  sid,
		Seq:  1,
		Code: uint32(codes.Unimplemented),
	}.Build()
	handle_status := func(err error) error {
		st, ok := status.FromError(err)
		res.SetCode(uint32(st.Code()))
		if ok {
			res.SetDesc(st.Message())
			return nil
		} else {
			return st.Err()
		}
	}
	handle_res := func(v any) error {
		payload, err := proto.Marshal(v.(proto.Message))
		if err != nil {
			res.SetCode(uint32(codes.Unknown))
			return err
		}

		res.SetPayload(payload)
		res.SetCode(uint32(codes.OK))
		return s.tx(ctx, res)
	}

	var desc *serviceDesc
	if i := int(req.GetMethodIndex()); i == 0 {
		desc = s.services[req.GetMethod()]
		if desc == nil {
			return s.tx(ctx, res)
		}
	} else if len(s.methods) <= i {
		return s.tx(ctx, res)
	} else {
		desc = s.methods[i]
	}

	if desc.method != nil {
		// TODO: use codec.
		res, err := desc.method.Handler(desc.impl, ctx, func(v any) error {
			return proto.Unmarshal(req.GetPayload(), v.(proto.Message))
		}, nil)
		if err != nil {
			return handle_status(err)
		}

		return handle_res(res)
	}

	stream, ok := s.getStream(sid)
	if !ok {
		s.wg.Go(func() {
			desc.stream.Handler(desc.impl, stream)
		})
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case stream.rx <- req:
	}

	return nil
}

func (s *Server) getStream(sid uint32) (*serverStream, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stream, ok := s.ss[sid]
	if !ok {
		stream = newServerStream(context.Background(), s, sid)
		s.ss[sid] = stream
	}

	return stream, ok
}

type serviceDesc struct {
	index    uint32
	fullname string

	service *grpc.ServiceDesc
	method  *grpc.MethodDesc
	stream  *grpc.StreamDesc

	impl any
}
