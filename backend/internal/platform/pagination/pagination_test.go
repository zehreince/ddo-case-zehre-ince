package pagination

import (
	"errors"
	"testing"
	"time"
)

func TestCursor_RoundTrip(t *testing.T) {
	in := Cursor{CreatedAt: time.Date(2026, 10, 30, 12, 0, 0, 123456000, time.UTC), ID: "abc"}
	out, err := Decode(in.Encode())
	if err != nil || out == nil || !out.CreatedAt.Equal(in.CreatedAt) || out.ID != in.ID {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestDecode(t *testing.T) {
	if c, err := Decode(""); c != nil || err != nil {
		t.Fatal("boş imleç ilk sayfa olmalı")
	}
	for _, bad := range []string{"!!!", "e30", Cursor{ID: "x"}.Encode()} {
		if _, err := Decode(bad); !errors.Is(err, ErrBadCursor) {
			t.Errorf("%q reddedilmeli: %v", bad, err)
		}
	}
}

func TestLimit(t *testing.T) {
	cases := map[string]int{"": 20, "1": 1, "100": 100}
	for in, want := range cases {
		if got, err := Limit(in, 20, 100); err != nil || got != want {
			t.Errorf("Limit(%q)=%d,%v", in, got, err)
		}
	}
	for _, bad := range []string{"0", "101", "-1", "abc"} {
		if _, err := Limit(bad, 20, 100); err == nil {
			t.Errorf("Limit(%q) reddedilmeli", bad)
		}
	}
}
