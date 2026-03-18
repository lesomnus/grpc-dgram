package echo_test

import (
	"testing"

	"github.com/lesomnu/grpc-dgram/internal/echo"
	"github.com/lesomnu/grpc-dgram/internal/x"
)

func TestCircularShift(t *testing.T) {
	x.Equal(t, "cab", echo.CircularShift("abc", -4))
	x.Equal(t, "abc", echo.CircularShift("abc", -3))
	x.Equal(t, "bca", echo.CircularShift("abc", -2))
	x.Equal(t, "cab", echo.CircularShift("abc", -1))
	x.Equal(t, "abc", echo.CircularShift("abc", +0))
	x.Equal(t, "bca", echo.CircularShift("abc", +1))
	x.Equal(t, "cab", echo.CircularShift("abc", +2))
	x.Equal(t, "abc", echo.CircularShift("abc", +3))
	x.Equal(t, "bca", echo.CircularShift("abc", +4))
}
