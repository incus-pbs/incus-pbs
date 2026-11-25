//go:build !linux

package pbsqemu

import "errors"

var errUnsupported = errors.New(LibName + " is only available on Linux")

// Session is unavailable on this platform.
type Session struct{ HasPrevious bool }

func Load() error                                     { return errUnsupported }
func Version() string                                 { return "" }
func ImportState([]byte) error                        { return errUnsupported }
func ExportState() ([]byte, error)                    { return nil, errUnsupported }
func New(Options) (*Session, error)                   { return nil, errUnsupported }
func (s *Session) CanIncremental(string, uint64) bool { return false }
func (s *Session) RegisterImage(string, uint64, bool) (uint8, error) {
	return 0, errUnsupported
}
func (s *Session) Write(uint8, uint64, []byte, uint64) (bool, error) { return false, errUnsupported }
func (s *Session) CloseImage(uint8) error                            { return errUnsupported }
func (s *Session) AddConfig(string, []byte) error                    { return errUnsupported }
func (s *Session) Finish() error                                     { return errUnsupported }
func (s *Session) Abort(string)                                      {}
func (s *Session) Close()                                            {}
