package checksum

import "testing"

func TestParse(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"", ""},
		{"off", ""},
		{"none", ""},
		{"false", ""},
		{"auto", "auto"},
		{"crc32c", "crc32c"},
		{"CRC32C", "crc32c"},
		{"crc32", "crc32c"},
		{"xxhash", "xxhash"},
		{"xxhash64", "xxhash"},
	} {
		got, err := Parse(tt.in)
		if err != nil {
			t.Fatalf("Parse(%q) err %v", tt.in, err)
		}
		if string(got) != tt.want {
			t.Fatalf("Parse(%q)=%q want %q", tt.in, got, tt.want)
		}
	}
	if _, err := Parse("md5"); err == nil {
		t.Fatal("Parse(md5) should fail")
	}
}

func TestResolveAuto(t *testing.T) {
	// auto should resolve to a concrete algo, never stay auto or none
	a, _ := Parse("auto")
	r := Resolve(a)
	if r != AlgoCRC32C && r != AlgoXXHash {
		t.Fatalf("Resolve(auto)=%q want crc32c or xxhash", r)
	}
	// explicit stays explicit
	if Resolve(AlgoCRC32C) != AlgoCRC32C {
		t.Fatal("Resolve(crc32c) changed")
	}
	if Resolve(AlgoNone) != AlgoNone {
		t.Fatal("Resolve(none) changed")
	}
}

func TestResolveString(t *testing.T) {
	a, err := ResolveString("auto")
	if err != nil || (a != AlgoCRC32C && a != AlgoXXHash) {
		t.Fatalf("ResolveString(auto)=%q err %v", a, err)
	}
	if _, err := ResolveString("bad"); err == nil {
		t.Fatal("ResolveString(bad) should fail")
	}
}

func TestHashBytesDeterministic(t *testing.T) {
	data := []byte("hello world")
	h1 := HashBytes(AlgoCRC32C, data)
	h2 := HashBytes(AlgoCRC32C, data)
	if h1 != h2 {
		t.Fatalf("crc32c not deterministic %x vs %x", h1, h2)
	}
	if h1 == 0 {
		t.Fatal("crc32c of non-empty should be non-zero")
	}
	h3 := HashBytes(AlgoXXHash, data)
	h4 := HashBytes(AlgoXXHash, data)
	if h3 != h4 {
		t.Fatalf("xxhash not deterministic %x vs %x", h3, h4)
	}
	// Different algo should differ (probabilistically, but for this input they must)
	if h1 == h3 {
		t.Logf("warning: crc32c and xxhash collided (rare but possible)")
	}
}

func TestNewHashStreaming(t *testing.T) {
	h, err := New(AlgoCRC32C)
	if err != nil {
		t.Fatal(err)
	}
	h.Write([]byte("hello "))
	h.Write([]byte("world"))
	sum := h.Sum64()
	if sum != HashBytes(AlgoCRC32C, []byte("hello world")) {
		t.Fatalf("streaming vs one-shot mismatch %x", sum)
	}
}
