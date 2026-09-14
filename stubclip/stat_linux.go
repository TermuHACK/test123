//go:build linux

package clipboard

import "os"

func statOK(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode()&0111 != 0
}

func environImpl() []string { return os.Environ() }
