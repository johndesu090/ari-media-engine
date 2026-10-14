package webrtc

import "testing"

func TestSelectRID(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"l", "m", "h"}, "h"},
		{[]string{"h", "m", "l"}, "h"},
		{[]string{"m", "l"}, "m"},
		{[]string{"l"}, "l"},
		{[]string{"low", "medium", "high"}, "high"},
		{[]string{"0", "1", "2"}, "2"},
		{[]string{"a", "b"}, "b"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := selectRID(c.in); got != c.want {
			t.Errorf("selectRID(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
