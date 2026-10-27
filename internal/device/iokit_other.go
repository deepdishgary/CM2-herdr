//go:build !(darwin && cgo)

package device

// Open is not available in this build.
func Open() (Transport, error) { return nil, ErrUnsupported }

// OpenWith is not available in this build.
func OpenWith(OpenOptions) (Transport, error) { return nil, ErrUnsupported }

// Access always reports AccessUnknown in this build.
func Access() AccessState { return AccessUnknown }

// RequestAccess does nothing in this build.
func RequestAccess() bool { return false }

// Devices is not available in this build.
func Devices() ([]DeviceInfo, error) { return nil, ErrUnsupported }
