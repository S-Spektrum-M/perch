package server

import (
	"strconv"

	"perchd/internal/pi"
)

type discardRPC struct{}

func (discardRPC) Write(p []byte) (int, error) { return len(p), nil }
func (discardRPC) Close() error                { return nil }

// piFeed passes one record from pi's stdout to a, as the pi backend would.
func piFeed(a *Agent, line []byte) {
	pi.NewRPC(discardRPC{}, a).HandleLine(line)
}

// piEvent feeds a pi event of type typ; body is a JSON object with its other
// fields, or nil.
func piEvent(a *Agent, typ string, body []byte) {
	line := `{"type":` + strconv.Quote(typ)
	if len(body) > 2 {
		line += "," + string(body[1:])
	} else {
		line += "}"
	}
	piFeed(a, []byte(line))
}
