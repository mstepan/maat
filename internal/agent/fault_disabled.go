//go:build !maat_faults

package agent

import "context"

func (*Runtime) fault(context.Context, string) error { return nil }
