package notice

import (
	"reflect"
	"testing"
)

func TestFold(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		{nil, []string{}},
		{[]string{"a", "", "a", "b"}, []string{"a", "b"}},
		{[]string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{[]string{"a", "b", "c", "d", "e"}, []string{"a", "b", "and 3 more"}},
	}
	for _, tc := range cases {
		if got := Fold(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Fold(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
