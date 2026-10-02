//go:build !linux

package hardware

import (
	"context"
	"errors"
	"github.com/damonto/euicc-go/driver"
)

func openQMICard(context.Context, string) (driver.SmartCardChannel, error) {
	return nil, errors.New("LINUX_REQUIRED")
}
