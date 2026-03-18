package federation

import "time"

// ExportNowFunc returns the current nowFunc for saving/restoring in tests.
func ExportNowFunc() func() time.Time { return nowFunc }

// SetNowFunc overrides the time source for testing staleness.
func SetNowFunc(fn func() time.Time) { nowFunc = fn }
