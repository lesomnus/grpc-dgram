package x

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func True(t *testing.T, condition bool, msgAndArgs ...interface{}) {
	t.Helper()
	if !condition {
		if len(msgAndArgs) > 0 {
			t.Fatalf("assert true failed: %s", fmt.Sprint(msgAndArgs...))
		}
		t.Fatal("assert true failed")
	}
}

func False(t *testing.T, condition bool, msgAndArgs ...interface{}) {
	t.Helper()
	if condition {
		if len(msgAndArgs) > 0 {
			t.Fatalf("assert false failed: %s", fmt.Sprint(msgAndArgs...))
		}
		t.Fatal("assert false failed")
	}
}

func Equal[T comparable](t *testing.T, expected, actual T, msgAndArgs ...interface{}) {
	t.Helper()
	if !reflect.DeepEqual(expected, actual) {
		if len(msgAndArgs) > 0 {
			t.Fatalf("assert equal failed: expected=%v actual=%v: %s", expected, actual, fmt.Sprint(msgAndArgs...))
		}
		t.Fatalf("assert equal failed: expected=%v actual=%v", expected, actual)
	}
}

func NotEqual[T comparable](t *testing.T, expected, actual T, msgAndArgs ...interface{}) {
	t.Helper()
	if reflect.DeepEqual(expected, actual) {
		if len(msgAndArgs) > 0 {
			t.Fatalf("assert not equal failed: both=%v: %s", expected, fmt.Sprint(msgAndArgs...))
		}
		t.Fatalf("assert not equal failed: both=%v", expected)
	}
}

func NoError(t *testing.T, err error, msgAndArgs ...interface{}) {
	t.Helper()
	if err != nil {
		if len(msgAndArgs) > 0 {
			t.Fatalf("assert no error failed: %v: %s", err, fmt.Sprint(msgAndArgs...))
		}
		t.Fatalf("assert no error failed: %v", err)
	}
}

func Error(t *testing.T, err error, msgAndArgs ...interface{}) {
	t.Helper()
	if err == nil {
		if len(msgAndArgs) > 0 {
			t.Fatalf("assert error failed: expected error: %s", fmt.Sprint(msgAndArgs...))
		}
		t.Fatal("assert error failed: expected error")
	}
}

func ErrorIs(t *testing.T, err, target error, msgAndArgs ...interface{}) {
	t.Helper()
	if errors.Is(err, target) {
		return
	}

	var expectedText string
	if target != nil {
		expectedText = target.Error()
		if err == nil {
			t.Fatalf("Expected error with %q in chain but got nil.: %s", expectedText, fmt.Sprint(msgAndArgs...))
			return
		}
	}

	chain := buildErrorChainString(err, false)

	t.Fatalf("Target error should be in err chain:\n"+
		"expected: %q\n"+
		"in chain: %s\n"+
		"%s", expectedText, chain,
		fmt.Sprint(msgAndArgs...))
	return
}

func buildErrorChainString(err error, withType bool) string {
	if err == nil {
		return ""
	}

	var chain string
	errs := unwrapAll(err)
	for i := range errs {
		if i != 0 {
			chain += "\n\t"
		}
		chain += fmt.Sprintf("%q", errs[i].Error())
		if withType {
			chain += fmt.Sprintf(" (%T)", errs[i])
		}
	}
	return chain
}

func unwrapAll(err error) (errs []error) {
	errs = append(errs, err)
	switch x := err.(type) {
	case interface{ Unwrap() error }:
		err = x.Unwrap()
		if err == nil {
			return
		}
		errs = append(errs, unwrapAll(err)...)
	case interface{ Unwrap() []error }:
		for _, err := range x.Unwrap() {
			errs = append(errs, unwrapAll(err)...)
		}
	}
	return
}
