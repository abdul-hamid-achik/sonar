//go:build !darwin && !linux

package db

import (
	"fmt"
	"os"
	"runtime"
)

func validateExecutionLeasePlatform() error {
	return fmt.Errorf("execution session leases are unsupported on %s", runtime.GOOS)
}

func openExecutionLeaseFile(string) (*os.File, error) {
	return nil, fmt.Errorf("execution session leases are unsupported on %s", runtime.GOOS)
}

func tryLockExecutionLeaseFile(*os.File) (bool, error) {
	return false, fmt.Errorf("execution session leases are unsupported on %s", runtime.GOOS)
}

func unlockExecutionLeaseFile(*os.File) error { return nil }
