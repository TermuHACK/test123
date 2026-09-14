//go:build !linux

package clipboard

import "errors"

func forkExecInput(bin, s string) (int, error)  { return 0, errors.New("unsupported") }
func forkExecOutput(bin string) (string, error) { return "", errors.New("unsupported") }
