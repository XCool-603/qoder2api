//go:build !windows

package cosy

func scanDesktopAppAuth(realm, domain, appDirName string) *LocalCredentialCandidate {
	return nil
}
