package config

// SetPlatformForTest makes Load check configs against goos (a test seam for other packages'
// tests; the in-package tests set platformGOOS directly). Test-only: never call it from product
// code. It returns the restore function.
func SetPlatformForTest(goos string) (restore func()) {
	old := platformGOOS
	platformGOOS = goos
	return func() { platformGOOS = old }
}
