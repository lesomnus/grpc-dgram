package drpc

import "google.golang.org/grpc/metadata"

func newMd(md metadata.MD) *Metadata {
	es := map[string]*Metadata_Entry{}
	for k, v := range md {
		es[k] = Metadata_Entry_builder{Values: v}.Build()
	}

	return Metadata_builder{Entries: es}.Build()
}
