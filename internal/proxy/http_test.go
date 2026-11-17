package proxy

import "testing"

func TestETagMatching(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		header string
		want   bool
	}{
		{`"current"`, true}, {`W/"current"`, true}, {`"other", W/"current"`, true},
		{`*`, true}, {`"other"`, false}, {"", false},
	} {
		if got := matchesETag(tt.header, `"current"`); got != tt.want {
			t.Errorf("%q: got %t want %t", tt.header, got, tt.want)
		}
	}
}
