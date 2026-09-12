package drpc

import (
	"encoding/hex"
	"reflect"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// The §5 metadata golden vectors, shared byte for byte with the TS suite
// (ts/src/wire.test.ts, "golden bytes"): what the core puts on the wire for
// each awkward metadata shape, on Frame{epoch: 1}. Ascending key order is a
// §11 MUST, and it is what makes these vectors reproducible on both sides
// without any marshal option.
func TestMetadata_GoldenBytes(t *testing.T) {
	cases := []struct {
		name, want string
		md         metadata.MD
		trailer    bool
	}{
		{"a -bin value carries raw octets", "0d0100000062100a0e0a05782d62696e12050001ff807f", metadata.MD{"x-bin": {"\x00\x01\xff\x80\x7f"}}, false},
		{"text values are their bytes, each its own field-2 element", "0d0100000062190a170a06782d74657874120568656c6c6f1206776f72206c64", metadata.MD{"x-text": {"hello", "wor ld"}}, false},
		{"a key with no values is its key alone", "0d0100000062070a050a03782d61", metadata.MD{"x-a": {}}, false},
		{"a key with one empty value is a zero-length element", "0d0100000062090a070a03782d611200", metadata.MD{"x-a": {""}}, false},
		{"keys go out ascending whatever the map order was", "0d010000006a1a0a0b0a06612d746578741201760a0b0a057a2d62696e1202ff00", metadata.MD{"z-bin": {"\xff\x00"}, "a-text": {"v"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := Frame_builder{Epoch: 1}
			if c.trailer {
				b.Trailer = newMd(c.md)
			} else {
				b.Header = newMd(c.md)
			}
			data, err := proto.Marshal(b.Build())
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(data); got != c.want {
				t.Fatalf("wire bytes:\n got  %s\n want %s", got, c.want)
			}
		})
	}
}

func TestMetadata_SenderEmitsAscendingKeyOrder(t *testing.T) {
	md := metadata.MD{"z": {"1"}, "a": {"2"}, "m-bin": {"3"}, "b.c": {"4"}, "a_": {"5"}}
	for i := 0; i < 20; i++ { // map iteration order is randomised; the wire order must not be
		var got []string
		for _, e := range newMd(md).GetEntries() {
			got = append(got, e.GetKey())
		}
		want := []string{"a", "a_", "b.c", "m-bin", "z"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("entry order %v, want ascending %v", got, want)
		}
	}
}

func TestMetadata_ReceiverMergesARepeatedKeyInWireOrder(t *testing.T) {
	// A sender never repeats a key (§11); a receiver merges one that does,
	// appending in wire order — never "last wins", metadata is a multimap.
	cases := []struct {
		name, wire string
		want       metadata.MD
	}{
		// the TS suite's vector: "x-a":["a"] then "x-a":["b"]
		{"adjacent", "0a080a03782d611201610a080a03782d61120162", metadata.MD{"x-a": {"a", "b"}}},
		// "x-a":["a"], "x-b":["b"], "x-a":["c"] — the merge is by key, not by adjacency
		{"interleaved", "0a080a03782d611201610a080a03782d621201620a080a03782d61120163", metadata.MD{"x-a": {"a", "c"}, "x-b": {"b"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := hex.DecodeString(c.wire)
			if err != nil {
				t.Fatal(err)
			}
			var m Metadata
			if err := proto.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			if got := m.MD(); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("MD() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestMetadata_KeyWithNoValuesIsPresentAndEmpty(t *testing.T) {
	raw, _ := hex.DecodeString("0a050a03782d61") // Metadata{entries:[{key:"x-a"}]}
	var m Metadata
	if err := proto.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	got := m.MD()
	v, ok := got["x-a"]
	if !ok || v == nil || len(v) != 0 {
		t.Fatalf(`MD()["x-a"] = %#v (present=%v); want a present, empty, non-nil slice`, v, ok)
	}
	// and it survives the round trip as the same wire shape
	data, err := proto.Marshal(newMd(got))
	if err != nil {
		t.Fatal(err)
	}
	if h := hex.EncodeToString(data); h != "0a050a03782d61" {
		t.Fatalf("round trip = %s, want 0a050a03782d61", h)
	}
}

// A proto `string` field that is not valid UTF-8 — a metadata key here, or
// any other — makes the envelope undecodable: proto.Unmarshal rejects it and
// every adapter drops what it cannot unmarshal (§5, §11). The TS decoder
// throws on the same bytes (ts/src/wire.test.ts), so the two receivers agree:
// neither surfaces a partial frame. Metadata VALUES are bytes and are not
// validated (§11).
func TestMetadata_InvalidUTF8StringFieldIsUndecodable(t *testing.T) {
	for name, wire := range map[string]string{
		"metadata key": "0d0100000062080a060a01ff120161", // Metadata{entries:[{key: ff, values:["a"]}]}
		"method":       "0d010000002a01ff",               // method = 0xff
	} {
		raw, err := hex.DecodeString(wire)
		if err != nil {
			t.Fatal(err)
		}
		if err := proto.Unmarshal(raw, &Frame{}); err == nil {
			t.Errorf("%s: an invalid-UTF-8 string field unmarshalled; want the envelope rejected", name)
		}
	}
	// A value is bytes: the same octet in a value is fine.
	raw, _ := hex.DecodeString("0a080a03782d611201ff")
	var m Metadata
	if err := proto.Unmarshal(raw, &m); err != nil {
		t.Fatalf("a non-UTF-8 metadata VALUE must decode (values are bytes, §11): %v", err)
	}
}
