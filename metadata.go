package drpc

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Metadata values travel as raw bytes (PROTOCOL.md §11): gRPC's binary
// metadata ("-bin" keys) carries arbitrary octets, which a proto string
// cannot hold. grpc-go keeps those octets in the string of a metadata.MD
// value, so the conversion here is a plain re-typing in both directions —
// no base64, no change of semantics. (The TS port, whose metadata values are
// JS strings, base64s "-bin" values at its own API boundary; the wire bytes
// are identical.)

func (x *Metadata) MD() metadata.MD {
	v := metadata.MD{}
	for k, e := range x.GetEntries() {
		bs := e.GetValues()
		ss := make([]string, len(bs))
		for i, b := range bs {
			ss[i] = string(b)
		}
		v[k] = ss
	}

	return v
}

func newMd(md metadata.MD) *Metadata {
	es := map[string]*Metadata_Entry{}
	for k, v := range md {
		bs := make([][]byte, len(v))
		for i, s := range v {
			bs[i] = []byte(s)
		}
		es[k] = Metadata_Entry_builder{Values: bs}.Build()
	}

	return Metadata_builder{Entries: es}.Build()
}

// validateMD mirrors grpc-go's internal/metadata.Validate, so the same
// metadata is legal on both stacks (PROTOCOL.md §11):
//   - a key is non-empty and drawn from [0-9 a-z _ - .];
//   - the values of a "-bin" key are arbitrary bytes, unvalidated;
//   - every other value is printable ASCII (%x20-%x7E).
//
// Validating here is what keeps a violation from surfacing as a proto
// marshal failure deep inside an adapter — a bare UNKNOWN naming no key.
func validateMD(md metadata.MD) error {
	for k, vs := range md {
		if err := validateMDPair(k, vs...); err != nil {
			return err
		}
	}
	return nil
}

func validateMDPair(key string, vals ...string) error {
	if key == "" {
		return fmt.Errorf("there is an empty key in the header")
	}
	for i := 0; i < len(key); i++ {
		r := key[i]
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '.' && r != '-' && r != '_' {
			return fmt.Errorf("header key %q contains illegal characters not in [0-9a-z-_.]", key)
		}
	}
	if strings.HasSuffix(key, "-bin") {
		// Binary metadata: any octets, carried verbatim by the bytes field.
		return nil
	}
	for _, v := range vals {
		for i := 0; i < len(v); i++ {
			if v[i] < 0x20 || v[i] > 0x7E {
				return fmt.Errorf("header key %q contains value with non-printable ASCII characters", key)
			}
		}
	}
	return nil
}

// mdStatusErr is how a metadata violation surfaces, matching grpc-go:
// codes.Internal carrying the validation message.
func mdStatusErr(err error) error {
	return status.Error(codes.Internal, err.Error())
}

func newIncomingContext(ctx context.Context, req *Frame) context.Context {
	h := req.GetHeader()
	if h == nil {
		return ctx
	}

	md := h.MD()
	return metadata.NewIncomingContext(ctx, md)
}
