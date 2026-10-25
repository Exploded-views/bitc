package provider

import (
	"errors"
	"testing"
)

func TestBoundaryNames(t *testing.T) {
	for _, c := range []struct {
		s string
		b Boundary
	}{{"resource", UntilResource}, {"attach", UntilAttach}} {
		got, ok := ParseBoundary(c.s)
		if !ok || got != c.b || c.b.String() != c.s {
			t.Errorf("%q: parsed %v %v, String %q", c.s, got, ok, c.b.String())
		}
	}
	if _, ok := ParseBoundary("later"); ok {
		t.Error("ParseBoundary accepted \"later\"")
	}
}

func TestBoundaryErrorsWrapTheirCause(t *testing.T) {
	inner := &ErrTransient{Why: "apt timeout"}
	for _, err := range []error{&ErrPreAttach{ID: "r1", Err: inner}, &ErrAttached{ID: "r1", Err: inner}} {
		var tr *ErrTransient
		if err.Error() != "apt timeout" || !errors.As(err, &tr) || tr != inner {
			t.Errorf("%T: %q, As %v", err, err.Error(), errors.As(err, &tr))
		}
	}
}
