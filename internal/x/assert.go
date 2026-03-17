package x

import (
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

func Equal(t *testing.T, expected, actual interface{}, msgAndArgs ...interface{}) {
	t.Helper()
	if !reflect.DeepEqual(expected, actual) {
		if len(msgAndArgs) > 0 {
			t.Fatalf("assert equal failed: expected=%v actual=%v: %s", expected, actual, fmt.Sprint(msgAndArgs...))
		}
		t.Fatalf("assert equal failed: expected=%v actual=%v", expected, actual)
	}
}

func NotEqual(t *testing.T, expected, actual interface{}, msgAndArgs ...interface{}) {
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
